package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/alertmanager/template"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
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
	a, err := NewWithClients(ctx, config("https://relay.example/"), kube, gwfake.NewSimpleClientset(), newDynamic())
	if err != nil {
		t.Fatal(err)
	}
	if a.pairing.RelayURL != "https://relay.example" || a.pairing.Cluster != "kind-dev" {
		t.Fatalf("%+v", a.pairing)
	}
	again, _ := NewWithClients(ctx, config("https://relay.example"), kube, gwfake.NewSimpleClientset(), newDynamic())
	if again.pairing != a.pairing {
		t.Fatal("keys changed on restart")
	}
	moved, _ := NewWithClients(ctx, config("https://other.example"), kube, gwfake.NewSimpleClientset(), newDynamic())
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
	a, err := NewWithClients(ctx, config(relay.URL), kube, gwfake.NewSimpleClientset(), newDynamic())
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

var (
	applicationsGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	certificatesGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
)

// newDynamic is the dynamic client for Argo CD and cert-manager objects.
func newDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		applicationsGVR: "ApplicationList", certificatesGVR: "CertificateList",
	}, objs...)
}

func argoApp(name, health string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": name, "namespace": "argocd"},
		"spec":     map[string]any{"destination": map[string]any{"namespace": "payments"}},
		"status":   map[string]any{"health": map[string]any{"status": health}, "sync": map[string]any{"status": "Synced"}},
	}}
}

func TestArgoCDIsWatchedOnceInstalledAndCertManagerSkippedWhenForbidden(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relay := newRelay(t)
	kube := kubefake.NewClientset()
	disco := kube.Discovery().(*fakediscovery.FakeDiscovery)
	disco.Resources = []*metav1.APIResourceList{
		{GroupVersion: "argoproj.io/v1alpha1", APIResources: []metav1.APIResource{{Name: "applications"}}},
		{GroupVersion: "cert-manager.io/v1", APIResources: []metav1.APIResource{{Name: "certificates"}}},
	}
	// Neither is installed when the agent starts.
	var installed atomic.Bool
	disco.PrependReactor("get", "resource", func(kubetesting.Action) (bool, runtime.Object, error) {
		if installed.Load() {
			return false, nil, nil
		}
		return true, nil, apierrors.NewNotFound(schema.GroupResource{}, "")
	})
	expired := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
		"metadata": map[string]any{"name": "api-tls", "namespace": "web"},
		"status":   map[string]any{"notAfter": "2020-01-01T00:00:00Z"},
	}}
	dyn := newDynamic(argoApp("fraud-detector", "Degraded"), expired)
	// An install from before cert-manager was read: not allowed.
	dyn.PrependReactor("list", "certificates", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(certificatesGVR.GroupResource(), "", nil)
	})
	cfg := config(relay.URL)
	cfg.Discover = 100 * time.Millisecond
	a, err := NewWithClients(ctx, cfg, kube, gwfake.NewSimpleClientset(), dyn)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Run(ctx) }()
	relay.waitFor(t, func(m PushMessage) bool { return m.ID == "clusterctl:hello" })

	installed.Store(true)
	fired := relay.waitFor(t, func(m PushMessage) bool {
		return m.ID == "applications/argocd/fraud-detector" && m.State == "firing"
	})
	if fired.Body != "fraud-detector is Degraded\nArgo CD · payments · Degraded" {
		t.Fatalf("body %q", fired.Body)
	}
	var inside struct {
		Alert rules.Alert `json:"alert"`
	}
	if err := pairing.Open(a.pairing.SealKey, fired.Sealed, &inside); err != nil || inside.Alert.Kind != rules.Applications ||
		inside.Alert.Source != rules.FromArgoCD || inside.Alert.Namespace != "argocd" || inside.Alert.Name != "fraud-detector" {
		t.Fatalf("sealed %+v %v", inside, err)
	}

	if _, err := dyn.Resource(applicationsGVR).Namespace("argocd").Update(ctx, argoApp("fraud-detector", "Healthy"),
		metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	relay.waitFor(t, func(m PushMessage) bool { return m.ID == "applications/argocd/fraud-detector" && m.State == "resolved" })

	relay.mu.Lock()
	defer relay.mu.Unlock()
	for _, m := range relay.got {
		if strings.HasPrefix(m.ID, "certificates/") {
			t.Fatalf("read a forbidden kind: %+v", m)
		}
	}
}

func TestParseMutes(t *testing.T) {
	got, err := parseMutes(`{"am:3f2a":"2026-10-31T13:00:00Z","applications/argocd/fraud-detector":"2026-11-01T08:00:00+01:00"}`)
	if err != nil || len(got) != 2 || !got["am:3f2a"].Equal(time.Date(2026, 10, 31, 13, 0, 0, 0, time.UTC)) ||
		!got["applications/argocd/fraud-detector"].Equal(time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := parseMutes(" "); got != nil || err != nil {
		t.Fatalf("empty: %v %v", got, err)
	}
	for _, bad := range []string{"nope", `{"am:1":"tomorrow"}`, `["am:1"]`} {
		if got, err := parseMutes(bad); got != nil || err == nil {
			t.Fatalf("%q: %v %v", bad, got, err)
		}
	}
}

func TestMutedAlertsAreTrackedButNotPushed(t *testing.T) {
	ctx := context.Background()
	mutes := func(until time.Time) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: configName, Namespace: "clusterctl-system"},
			Data:       map[string]string{"details": "true", "mutes": `{"am:f1":"` + until.UTC().Format(time.RFC3339) + `"}`},
		}
	}
	kube := kubefake.NewClientset(mutes(time.Now().Add(time.Hour)))
	a, err := NewWithClients(ctx, config("https://relay.example"), kube, gwfake.NewSimpleClientset(), newDynamic())
	if err != nil {
		t.Fatal(err)
	}
	a.readConfig(ctx)
	pushed := func() []string {
		var ids []string
		for _, m := range a.relay.queue {
			ids = append(ids, m.ID+" "+m.State)
		}
		a.relay.queue = nil
		return ids
	}
	crash := rules.Alert{ID: "am:f1", Title: "api-1 crash loops", Severity: rules.Critical, Source: rules.FromAlertmanager}
	other := rules.Alert{ID: "am:f2", Title: "disk filling", Severity: rules.Warning, Source: rules.FromAlertmanager}

	// Muted: still tracked, not pushed; others are.
	a.alertmanager(ctx, []AmAlert{{Alert: crash, Firing: true}, {Alert: other, Firing: true}})
	if got := pushed(); len(got) != 1 || got[0] != "am:f2 firing" {
		t.Fatalf("pushed %v", got)
	}
	if _, ok := a.amActive["am:f1"]; !ok {
		t.Fatal("muted alert not tracked")
	}

	// The mute ends: still firing isn't news, resolving is.
	if _, err := kube.CoreV1().ConfigMaps("clusterctl-system").Update(ctx, mutes(time.Now().Add(-time.Minute)),
		metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	a.readConfig(ctx)
	a.alertmanager(ctx, []AmAlert{{Alert: crash, Firing: true}})
	if got := pushed(); len(got) != 0 {
		t.Fatalf("re-pushed after the mute: %v", got)
	}
	a.alertmanager(ctx, []AmAlert{{Alert: crash}})
	if got := pushed(); len(got) != 1 || got[0] != "am:f1 resolved" {
		t.Fatalf("pushed %v", got)
	}

	// Malformed: no mutes.
	cm := mutes(time.Now().Add(time.Hour))
	cm.Data["mutes"] = "{"
	if _, err := kube.CoreV1().ConfigMaps("clusterctl-system").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	a.readConfig(ctx)
	a.alertmanager(ctx, []AmAlert{{Alert: crash, Firing: true}})
	if got := pushed(); len(got) != 1 || got[0] != "am:f1 firing" || a.badMutes != "{" {
		t.Fatalf("pushed %v", got)
	}
}
