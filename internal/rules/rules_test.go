package rules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// The clusterctl app runs these same cases (testdata/rules), so the agent and
// the app agree on what counts as a problem and how it's worded.
type ruleCase struct {
	Name    string                       `json:"name"`
	Objects map[string][]json.RawMessage `json:"objects"`
	Alerts  []struct {
		ID       string   `json:"id"`
		Title    string   `json:"title"`
		Detail   string   `json:"detail"`
		Severity Severity `json:"severity"`
		Affected []string `json:"affected"`
	} `json:"alerts"`
}

func decodeAll[T any](t *testing.T, raws []json.RawMessage) []*T {
	t.Helper()
	out := make([]*T, 0, len(raws))
	for _, r := range raws {
		v := new(T)
		if err := json.Unmarshal(r, v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func objectsOf(t *testing.T, raw map[string][]json.RawMessage) Objects {
	var o Objects
	for kind, items := range raw {
		switch Kind(kind) {
		case Pods:
			o.Pods = decodeAll[corev1.Pod](t, items)
		case Nodes:
			o.Nodes = decodeAll[corev1.Node](t, items)
		case PVCs:
			o.PVCs = decodeAll[corev1.PersistentVolumeClaim](t, items)
		case Deployments:
			o.Deployments = decodeAll[appsDeployment](t, items)
		case StatefulSets:
			o.StatefulSets = decodeAll[appsStatefulSet](t, items)
		case DaemonSets:
			o.DaemonSets = decodeAll[appsDaemonSet](t, items)
		case Jobs:
			o.Jobs = decodeAll[batchJob](t, items)
		case Gateways:
			o.Gateways = decodeAll[gwGateway](t, items)
		case HTTPRoutes:
			o.HTTPRoutes = decodeAll[gwHTTPRoute](t, items)
		default:
			t.Fatalf("unknown kind %q", kind)
		}
	}
	return o
}

func TestSharedCases(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/rules/*.json")
	if len(files) == 0 {
		t.Fatal("no cases in testdata/rules")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var c ruleCase
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		t.Run(c.Name, func(t *testing.T) {
			got := Problems(Rows(objectsOf(t, c.Objects)), time.Now())
			type flat struct {
				ID, Title, Detail string
				Severity          Severity
				Affected          []string
			}
			var g, w []flat
			for _, a := range got {
				g = append(g, flat{a.ID, a.Title, a.Detail, a.Severity, a.Affected})
			}
			for _, a := range c.Alerts {
				w = append(w, flat{a.ID, a.Title, a.Detail, a.Severity, a.Affected})
			}
			byID := func(s []flat) { sort.Slice(s, func(i, j int) bool { return s[i].ID < s[j].ID }) }
			byID(g)
			byID(w)
			if !reflect.DeepEqual(g, w) {
				t.Errorf("got  %+v\nwant %+v", g, w)
			}
		})
	}
}

func crashing(name string) Row {
	return Row{Kind: Pods, Name: name, Namespace: "shop", Health: Error, Status: "CrashLoopBackOff", Restarts: 5,
		Labels: map[string]string{"app": name}}
}

var all = map[Kind]bool{Pods: true, Nodes: true, Deployments: true}

func TestProblemsMustLastBeforeTheyFireAndResolveWhenGone(t *testing.T) {
	e := NewEngine(DefaultTiming)
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	rows := []Row{crashing("solo")}
	if ch := e.Update(rows, t0, all, false); len(ch.Fired) != 0 {
		t.Fatal("fired immediately")
	}
	if ch := e.Update(rows, t0.Add(10*time.Second), all, false); len(ch.Fired) != 0 {
		t.Fatal("fired too early")
	}
	ch := e.Update(rows, t0.Add(31*time.Second), all, false)
	if len(ch.Fired) != 1 || !ch.Fired[0].Since.Equal(t0) {
		t.Fatalf("fired %+v", ch.Fired)
	}
	if ch := e.Update(rows, t0.Add(time.Minute), all, false); !ch.Empty() {
		t.Fatalf("refired %+v", ch)
	}
	// Gone, but only resolved once it stays gone.
	if ch := e.Update(nil, t0.Add(2*time.Minute), all, false); len(ch.Resolved) != 0 {
		t.Fatal("resolved immediately")
	}
	if ch := e.Update(nil, t0.Add(5*time.Minute), all, false); len(ch.Resolved) != 1 || ch.Resolved[0].ID != "pods/shop/solo" {
		t.Fatalf("resolved %+v", ch.Resolved)
	}
}

func TestACrashLoopsHealthyMomentsDontResolveIt(t *testing.T) {
	e := NewEngine(DefaultTiming)
	t0 := time.Now()
	e.Update([]Row{crashing("flaky")}, t0, all, true)
	for i := 1; i <= 10; i++ {
		at := t0.Add(time.Duration(i) * 2 * time.Minute)
		rows := []Row{crashing("flaky")}
		if i%2 == 1 {
			rows = nil // healthy for a moment
		}
		if ch := e.Update(rows, at, all, false); !ch.Empty() {
			t.Fatalf("flapped at %d: %+v", i, ch)
		}
	}
}

func TestBlipsShorterThanTheDelayNeverFire(t *testing.T) {
	e := NewEngine(DefaultTiming)
	t0 := time.Now()
	e.Update([]Row{crashing("p")}, t0, all, false)
	e.Update(nil, t0.Add(20*time.Second), all, false)
	if ch := e.Update([]Row{crashing("p")}, t0.Add(40*time.Second), all, false); len(ch.Fired) != 0 {
		t.Fatal("blip fired")
	}
}

func TestWarningsWaitLonger(t *testing.T) {
	e := NewEngine(DefaultTiming)
	t0 := time.Now()
	stuck := []Row{{Kind: Pods, Name: "p", Namespace: "shop", Health: Pending, Status: "Pending"}}
	e.Update(stuck, t0, all, false)
	if ch := e.Update(stuck, t0.Add(4*time.Minute), all, false); len(ch.Fired) != 0 {
		t.Fatal("warning fired early")
	}
	if ch := e.Update(stuck, t0.Add(5*time.Minute), all, false); len(ch.Fired) != 1 || ch.Fired[0].Severity != Warning {
		t.Fatalf("%+v", ch)
	}
}

func TestBaselineAndUnloadedKinds(t *testing.T) {
	e := NewEngine(DefaultTiming)
	if ch := e.Update([]Row{crashing("p")}, time.Now(), all, true); len(ch.Fired) != 0 || len(e.Active()) != 1 {
		t.Fatal("baseline should be quiet but active")
	}
	// Pods failed to load: their alerts must not resolve.
	if ch := e.Update(nil, time.Now().Add(time.Hour), map[Kind]bool{Nodes: true}, false); len(ch.Resolved) != 0 {
		t.Fatal("resolved an unloaded kind")
	}
}

func TestAlertJSONMatchesTheApp(t *testing.T) {
	a := Alert{ID: "pods/shop/p", Title: "t", Detail: "d", Severity: Critical, Source: FromClusterctl, Kind: Pods,
		Namespace: "shop", Name: "p", Since: time.Date(2026, 10, 9, 21, 0, 37, 375662000, time.UTC)}
	b, _ := json.Marshal(a)
	want := `{"id":"pods/shop/p","title":"t","detail":"d","severity":"critical","since":"2026-10-09T21:00:37.375662Z","source":"clusterctl","kind":"pods","namespace":"shop","name":"p"}`
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
	var back Alert
	if err := json.Unmarshal(b, &back); err != nil || !back.Since.Equal(a.Since) || back.Kind != Pods {
		t.Fatalf("%+v %v", back, err)
	}
}
