package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/alertmanager/template"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	gwfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"

	"github.com/RazaLabs/clusterctl-agent/internal/pairing"
	"github.com/RazaLabs/clusterctl-agent/internal/rules"
)

// fakeRelay records what the agent pushes.
type fakeRelay struct {
	*httptest.Server
	mu   sync.Mutex
	got  []PushMessage
	auth string
}

func newRelay(t *testing.T) *fakeRelay {
	r := &fakeRelay{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct{ Messages []PushMessage }
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.mu.Lock()
		r.got = append(r.got, body.Messages...)
		r.auth = req.Header.Get("Authorization")
		r.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(r.Close)
	return r
}

// waitFor polls until a push matching want arrives.
func (r *fakeRelay) waitFor(t *testing.T, want func(PushMessage) bool) PushMessage {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, m := range r.got {
			if want(m) {
				r.mu.Unlock()
				return m
			}
		}
		r.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t.Fatalf("no matching push; got %+v", r.got)
	return PushMessage{}
}

func crashingPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: map[string]string{"app": name}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "c", RestartCount: 6, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}
}

func config(relay string) Config {
	return Config{Namespace: "clusterctl-system", RelayURL: relay, ClusterName: "kind-dev", Addr: "127.0.0.1:0",
		Timing: rules.Timing{}, Tick: 200 * time.Millisecond}
}

func TestPairingKeysAreCreatedOnceAndFollowTheDeployment(t *testing.T) {
	ctx := context.Background()
	kube := kubefake.NewClientset()
	a, err := NewWithClients(ctx, config("https://relay.example/"), kube, gwfake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	if a.pairing.RelayURL != "https://relay.example" || a.pairing.Cluster != "kind-dev" {
		t.Fatalf("%+v", a.pairing)
	}
	again, _ := NewWithClients(ctx, config("https://relay.example"), kube, gwfake.NewSimpleClientset())
	if again.pairing != a.pairing {
		t.Fatal("keys changed on restart")
	}
	moved, _ := NewWithClients(ctx, config("https://other.example"), kube, gwfake.NewSimpleClientset())
	if moved.pairing.RelayKey != a.pairing.RelayKey || moved.pairing.RelayURL != "https://other.example" {
		t.Fatalf("%+v", moved.pairing)
	}
	s, _ := kube.CoreV1().Secrets("clusterctl-system").Get(ctx, "clusterctl-agent", metav1.GetOptions{})
	if p, _ := pairing.ParseCode(string(s.Data["pairing-code"])); p != moved.pairing {
		t.Fatal("pairing code not updated")
	}
}

func TestAlertsFlowToTheRelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relay := newRelay(t)
	kube := kubefake.NewClientset(crashingPod("old"))
	a, err := NewWithClients(ctx, config(relay.URL), kube, gwfake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Run(ctx) }()

	// First look: existing problems are summed up once, not announced one by one.
	hello := relay.waitFor(t, func(m PushMessage) bool { return m.ID == "clusterctl:hello" })
	if !strings.Contains(hello.Body, "1 problem right now: old keeps crashing") || hello.Title != "kind-dev" {
		t.Fatalf("hello %+v", hello)
	}
	if relay.auth != "Bearer "+a.pairing.RelayKey {
		t.Fatal("relay key not sent")
	}

	// A new problem fires, sealed for the phone.
	if _, err := kube.CoreV1().Pods("shop").Create(ctx, crashingPod("new"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	fired := relay.waitFor(t, func(m PushMessage) bool { return m.ID == "pods/shop/new" && m.State == "firing" })
	if fired.Body != "new keeps crashing\nPod in shop · CrashLoopBackOff · 6 restarts" {
		t.Fatalf("body %q", fired.Body)
	}
	var inside struct {
		Cluster string      `json:"cluster"`
		State   string      `json:"state"`
		Alert   rules.Alert `json:"alert"`
	}
	if err := pairing.Open(a.pairing.SealKey, fired.Sealed, &inside); err != nil || inside.Alert.Title != "new keeps crashing" ||
		inside.Cluster != a.pairing.ClusterID() {
		t.Fatalf("sealed %+v %v", inside, err)
	}

	// Gone: resolved.
	if err := kube.CoreV1().Pods("shop").Delete(ctx, "new", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	relay.waitFor(t, func(m PushMessage) bool { return m.ID == "pods/shop/new" && m.State == "resolved" })

	// The app reads problems and a heartbeat from the state ConfigMap.
	deadline := time.Now().Add(5 * time.Second)
	for {
		cm, err := kube.CoreV1().ConfigMaps("clusterctl-system").Get(ctx, "clusterctl-agent-state", metav1.GetOptions{})
		if err == nil && cm.Data["heartbeat"] != "" && strings.Contains(cm.Data["alerts"], "pods/shop/old") &&
			!strings.Contains(cm.Data["alerts"], "pods/shop/new") && cm.Data["version"] == Version {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state %+v %v", cm, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAlertmanagerWebhook(t *testing.T) {
	a := &Agent{amIn: make(chan []AmAlert, 1)}
	body, _ := json.Marshal(template.Data{Alerts: template.Alerts{{
		Status:      "firing",
		Labels:      template.KV{"alertname": "KubePodCrashLooping", "severity": "critical", "namespace": "shop", "pod": "api-1"},
		Annotations: template.KV{"summary": "Pod is crash looping."},
		Fingerprint: "f1",
	}}})
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/alertmanager", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	got := <-a.amIn
	al := got[0].Alert
	if !got[0].Firing || al.ID != "am:f1" || al.Title != "Pod is crash looping." || al.Severity != rules.Critical ||
		al.Detail != "Alertmanager · KubePodCrashLooping · shop / api-1" || al.Source != rules.FromAlertmanager {
		t.Fatalf("%+v", got[0])
	}

	rec = httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/alertmanager", strings.NewReader("nope")))
	if rec.Code != 400 {
		t.Fatalf("bad body accepted: %d", rec.Code)
	}
}

func TestFingerprintFallbackIsStable(t *testing.T) {
	labels := template.KV{"alertname": "DiskFilling", "node": "n1"}
	one := FromAlertmanager(template.Data{Alerts: template.Alerts{{Labels: labels}}})
	two := FromAlertmanager(template.Data{Alerts: template.Alerts{{Labels: template.KV{"node": "n1", "alertname": "DiskFilling"}}}})
	if one[0].Alert.ID != two[0].Alert.ID || one[0].Alert.Title != "DiskFilling" || one[0].Alert.Severity != rules.Warning {
		t.Fatalf("%+v %+v", one, two)
	}
}

func TestPrivateNotificationsHideDetails(t *testing.T) {
	p := pairing.Generate("https://r", "prod")
	a := rules.Alert{ID: "pods/x/p", Title: "p ran out of memory", Detail: "d", Severity: rules.Critical, Since: time.Now()}
	open, _ := pushFor(p, a, false, true)
	hidden, _ := pushFor(p, a, false, false)
	resolved, _ := pushFor(p, a, true, true)
	if open.Body != "p ran out of memory\nd" || hidden.Body != "Critical problem. Open clusterctl to see it." ||
		resolved.Body != "Resolved: p ran out of memory\nd" || resolved.State != "resolved" {
		t.Fatalf("%q %q %q", open.Body, hidden.Body, resolved.Body)
	}
}
