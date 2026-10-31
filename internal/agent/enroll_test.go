package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	gwfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
)

// fakeMembers answers enrollments: the first `fail` with failure (a dropped connection, or
// status), then status ok.
type fakeMembers struct {
	*httptest.Server
	mu     sync.Mutex
	got    []map[string]string
	fail   int
	drop   bool // fail by closing the connection
	status int  // the failure's status
}

func newMembers(t *testing.T) *fakeMembers {
	m := &fakeMembers{status: http.StatusServiceUnavailable}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/clusterctl/enroll" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.got = append(m.got, body)
		failing := len(m.got) <= m.fail
		drop, status := m.drop, m.status
		m.mu.Unlock()
		switch {
		case failing && drop:
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		case failing:
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		case body["key"] != "cck_good":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_key"}`))
		default:
			_, _ = w.Write([]byte(`{"org":"org1","orgName":"Acme"}`))
		}
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *fakeMembers) calls() []map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]string(nil), m.got...)
}

func enrollingAgent(t *testing.T, kube kubernetes.Interface, members, key string) *Agent {
	t.Helper()
	cfg := config("https://relay.example")
	cfg.EnrollKey, cfg.MembersURL = key, members
	a, err := NewWithClients(context.Background(), cfg, kube, gwfake.NewSimpleClientset(), newDynamic())
	if err != nil {
		t.Fatal(err)
	}
	a.enrollBackoff = 10 * time.Millisecond
	return a
}

func remembered(t *testing.T, kube kubernetes.Interface) enrollment {
	t.Helper()
	cm, err := kube.CoreV1().ConfigMaps("clusterctl-system").Get(context.Background(), stateName, metav1.GetOptions{})
	if err != nil {
		return enrollment{}
	}
	var e enrollment
	_ = json.Unmarshal([]byte(cm.Data[enrolledKey]), &e)
	return e
}

func TestEnrollsOncePerRelayKey(t *testing.T) {
	m := newMembers(t)
	kube := kubefake.NewClientset()
	a := enrollingAgent(t, kube, m.URL, "cck_good")
	a.enroll(context.Background())

	got := m.calls()
	if len(got) != 1 {
		t.Fatalf("calls %v", got)
	}
	if got[0]["key"] != "cck_good" || got[0]["cluster"] != a.pairing.ClusterID() || got[0]["name"] != "kind-dev" {
		t.Fatalf("body %v", got[0])
	}
	if len(got[0]["cluster"]) != 32 {
		t.Fatalf("cluster id %q: 16 bytes, hex", got[0]["cluster"])
	}
	e := remembered(t, kube)
	if e.Cluster != a.pairing.ClusterID() || e.OrgName != "Acme" || e.Org != "org1" || e.Key == "" || e.Key == "cck_good" {
		t.Fatalf("remembered %+v", e)
	}

	// The state the agent writes keeps it.
	a.writeState(context.Background(), true)
	if remembered(t, kube).Cluster == "" {
		t.Fatal("writing state lost the enrollment")
	}

	// A restart doesn't ask again.
	again := enrollingAgent(t, kube, m.URL, "cck_good")
	again.enroll(context.Background())
	if n := len(m.calls()); n != 1 {
		t.Fatalf("enrolled again on restart: %d calls", n)
	}

	// New pairing keys (a new relay key) enroll again.
	_ = kube.CoreV1().Secrets("clusterctl-system").Delete(context.Background(), secretName, metav1.DeleteOptions{})
	rekeyed := enrollingAgent(t, kube, m.URL, "cck_good")
	rekeyed.enroll(context.Background())
	if got := m.calls(); len(got) != 2 || got[1]["cluster"] != rekeyed.pairing.ClusterID() || got[1]["cluster"] == a.pairing.ClusterID() {
		t.Fatalf("after new keys: %v", got)
	}
}

func TestEnrollingRetriesWhenMembersIsAway(t *testing.T) {
	for _, drop := range []bool{true, false} {
		m := newMembers(t)
		m.fail, m.drop = 2, drop
		kube := kubefake.NewClientset()
		a := enrollingAgent(t, kube, m.URL, "cck_good")
		a.enroll(context.Background())
		if n := len(m.calls()); n != 3 {
			t.Fatalf("drop %v: %d calls", drop, n)
		}
		if remembered(t, kube).OrgName != "Acme" {
			t.Fatalf("drop %v: not remembered", drop)
		}
	}
}

func TestARefusedEnrollKeyIsNotRetried(t *testing.T) {
	m := newMembers(t)
	kube := kubefake.NewClientset()
	a := enrollingAgent(t, kube, m.URL, "cck_revoked")
	done := make(chan struct{})
	go func() { a.enroll(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("still retrying a refused key")
	}
	if n := len(m.calls()); n != 1 {
		t.Fatalf("%d calls", n)
	}
	if remembered(t, kube).Cluster != "" {
		t.Fatal("remembered a failed enrollment")
	}
}

func TestNoEnrollKeyNoEnrollment(t *testing.T) {
	m := newMembers(t)
	a := enrollingAgent(t, kubefake.NewClientset(), m.URL, "")
	a.enroll(context.Background())
	if n := len(m.calls()); n != 0 {
		t.Fatalf("%d calls", n)
	}
}

func TestEnrollingStopsWithTheAgent(t *testing.T) {
	m := newMembers(t)
	m.fail = 1 << 30
	a := enrollingAgent(t, kubefake.NewClientset(), m.URL, "cck_good")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { a.enroll(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("enroll didn't stop")
	}
}
