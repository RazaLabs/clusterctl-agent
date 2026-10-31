package rules

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// object decodes a sample object as the dynamic client returns it.
func object(t *testing.T, js string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(js), &u.Object); err != nil {
		t.Fatal(err)
	}
	return u
}

func application(name, status string) string {
	return `{"apiVersion":"argoproj.io/v1alpha1","kind":"Application",
		"metadata":{"name":"` + name + `","namespace":"argocd"},
		"spec":{"destination":{"namespace":"payments"}},"status":` + status + `}`
}

// want is a problem's wording; an empty Title means no problem.
type want struct {
	Title, Detail string
	Severity      Severity
}

func check(t *testing.T, kind Kind, js string, now time.Time, w want) {
	t.Helper()
	o := Objects{}
	switch kind {
	case Applications:
		o.Applications = []*unstructured.Unstructured{object(t, js)}
	case Certificates:
		o.Certificates = []*unstructured.Unstructured{object(t, js)}
	}
	got := Problems(Rows(o), now)
	if w.Title == "" {
		if len(got) != 0 {
			t.Fatalf("unexpected %+v", got)
		}
		return
	}
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	g := got[0]
	if (want{g.Title, g.Detail, g.Severity}) != w {
		t.Errorf("got  %+v\nwant %+v", want{g.Title, g.Detail, g.Severity}, w)
	}
}

func TestArgoCDApplications(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name, status string
		want         want
	}{
		{"healthy and synced", `{"health":{"status":"Healthy"},"sync":{"status":"Synced"},
			"operationState":{"phase":"Succeeded"}}`, want{}},
		{"progressing is a rollout, not a problem", `{"health":{"status":"Progressing"},"sync":{"status":"Synced"}}`, want{}},
		{"degraded names the unhealthy resource", `{"health":{"status":"Degraded"},"sync":{"status":"Synced"},
			"resources":[
				{"kind":"Service","name":"fraud-detector","health":{"status":"Healthy"}},
				{"kind":"Deployment","name":"fraud-detector","health":{"status":"Degraded",
					"message":"Deployment \"fraud-detector\" exceeded its progress deadline"}}]}`,
			want{"fraud-detector is Degraded",
				`Argo CD · payments · Deployment fraud-detector: Deployment "fraud-detector" exceeded its progress deadline`, Critical}},
		{"degraded without a message falls back to conditions", `{"health":{"status":"Degraded"},
			"conditions":[{"type":"SyncWarning","message":"ignored"},{"type":"ComparisonError","message":"repo unreachable"}]}`,
			want{"fraud-detector is Degraded", "Argo CD · payments · repo unreachable", Critical}},
		{"degraded with nothing to say", `{"health":{"status":"Degraded"}}`,
			want{"fraud-detector is Degraded", "Argo CD · payments · Degraded", Critical}},
		{"failed sync", `{"health":{"status":"Healthy"},"sync":{"status":"OutOfSync"},
			"operationState":{"phase":"Failed","message":"one or more objects failed to apply, reason: admission webhook denied\nmore"}}`,
			want{"fraud-detector failed to sync",
				"Argo CD · payments · one or more objects failed to apply, reason: admission webhook denied", Warning}},
		{"sync error without a message", `{"sync":{"status":"OutOfSync"},"operationState":{"phase":"Error"}}`,
			want{"fraud-detector failed to sync", "Argo CD · payments · Sync failed", Warning}},
		{"synced again after a failure", `{"health":{"status":"Healthy"},"sync":{"status":"Synced"},
			"operationState":{"phase":"Failed","message":"old"}}`, want{}},
		{"retrying a sync", `{"sync":{"status":"OutOfSync"},"operationState":{"phase":"Running"}}`, want{}},
		{"degraded wins over a failed sync", `{"health":{"status":"Degraded","message":"crashing"},"sync":{"status":"OutOfSync"},
			"operationState":{"phase":"Failed","message":"nope"}}`,
			want{"fraud-detector is Degraded", "Argo CD · payments · crashing", Critical}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check(t, Applications, application("fraud-detector", c.status), now, c.want)
		})
	}
}

func certificate(status string) string {
	return `{"apiVersion":"cert-manager.io/v1","kind":"Certificate",
		"metadata":{"name":"api-tls","namespace":"web"},"status":` + status + `}`
}

func TestCertManagerCertificates(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	ready := `{"type":"Ready","status":"True","reason":"Ready","message":"Certificate is up to date and has not expired"}`
	cases := []struct {
		name, status string
		want         want
	}{
		{"valid for weeks", `{"notAfter":"2026-12-30T12:00:00Z","conditions":[` + ready + `]}`, want{}},
		{"expires in 6 days", `{"notAfter":"2026-11-06T15:00:00Z","conditions":[` + ready + `]}`,
			want{"api-tls expires in 6 days", "cert-manager · web · valid until 6 Nov 2026", Warning}},
		{"expires tomorrow", `{"notAfter":"2026-11-01T13:00:00Z","conditions":[` + ready + `]}`,
			want{"api-tls expires in 1 day", "cert-manager · web · valid until 1 Nov 2026", Warning}},
		{"expires within hours", `{"notAfter":"2026-10-31T18:00:00Z","conditions":[` + ready + `]}`,
			want{"api-tls expires in less than a day", "cert-manager · web · valid until 31 Oct 2026", Warning}},
		{"expired", `{"notAfter":"2026-10-30T12:00:00Z","conditions":[{"type":"Ready","status":"False","reason":"Expired"}]}`,
			want{"api-tls has expired", "cert-manager · web · valid until 30 Oct 2026", Critical}},
		{"renewal failed", `{"notAfter":"2026-12-30T12:00:00Z","lastFailureTime":"2026-10-31T10:00:00Z","conditions":[
			{"type":"Ready","status":"False","reason":"Failed","message":"Issuing certificate as Secret was previously issued by Issuer"},
			{"type":"Issuing","status":"False","reason":"Failed","message":"The certificate request has failed to complete and will be retried: ACME order failed"}]}`,
			want{"api-tls can’t renew",
				"cert-manager · web · The certificate request has failed to complete and will be retried: ACME order failed", Warning}},
		{"first issuance failing", `{"lastFailureTime":"2026-10-31T10:00:00Z","conditions":[
			{"type":"Ready","status":"False","reason":"DoesNotExist","message":"Issuer letsencrypt not found"}]}`,
			want{"api-tls can’t be issued", "cert-manager · web · Issuer letsencrypt not found", Warning}},
		{"being issued is setup, not a problem", `{"conditions":[{"type":"Ready","status":"False","reason":"DoesNotExist"},
			{"type":"Issuing","status":"True","reason":"DoesNotExist"}]}`, want{}},
		{"renewed after a failure", `{"notAfter":"2027-01-29T12:00:00Z","lastFailureTime":"2026-10-31T10:00:00Z","conditions":[` + ready + `]}`,
			want{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check(t, Certificates, certificate(c.status), now, c.want)
		})
	}
}

func TestOptionalKindsAreLinkedAndResolveLikeTheRest(t *testing.T) {
	e := NewEngine(Timing{})
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	kinds := map[Kind]bool{Applications: true}
	app := func(health string) []Row {
		return Rows(Objects{Applications: []*unstructured.Unstructured{
			object(t, application("fraud-detector", `{"health":{"status":"`+health+`"},"sync":{"status":"Synced"}}`)),
		}})
	}
	ch := e.Update(app("Degraded"), t0, kinds, false)
	if len(ch.Fired) != 1 {
		t.Fatalf("%+v", ch)
	}
	b, _ := json.Marshal(ch.Fired[0])
	wantJSON := `{"id":"applications/argocd/fraud-detector","title":"fraud-detector is Degraded",` +
		`"detail":"Argo CD · payments · Degraded","severity":"critical","since":"2026-10-31T12:00:00.000000Z",` +
		`"source":"argocd","kind":"applications","namespace":"argocd","name":"fraud-detector"}`
	if string(b) != wantJSON {
		t.Fatalf("got %s", b)
	}
	// Restored from the state ConfigMap, it keeps its source and still resolves.
	var back Alert
	if err := json.Unmarshal(b, &back); err != nil || back.Source != FromArgoCD {
		t.Fatalf("%+v %v", back, err)
	}
	e = NewEngine(Timing{})
	e.Restore([]Alert{back})
	// Argo CD not loaded (yet): its alert must stay.
	if ch := e.Update(nil, t0.Add(time.Minute), map[Kind]bool{}, false); len(ch.Resolved) != 0 {
		t.Fatal("resolved an unloaded kind")
	}
	if ch := e.Update(app("Healthy"), t0.Add(2*time.Minute), kinds, false); len(ch.Resolved) != 1 {
		t.Fatalf("%+v", ch)
	}
}

func TestLongMessagesAreShortened(t *testing.T) {
	got := short(strings.Repeat("x", 300) + "\nsecond line")
	if len([]rune(got)) != 120 || !strings.HasSuffix(got, "…") {
		t.Fatalf("%q", got)
	}
}
