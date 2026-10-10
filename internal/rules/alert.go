// Package rules decides what counts as a problem on a cluster and words it
// for a phone notification. It must agree with the kubctl app, which runs
// the same checks on live data: testdata/rules holds cases both test against.
package rules

import (
	"encoding/json"
	"time"
)

// Kind names match the app's ResourceKind names, which appear in alert ids
// and in the alert JSON the app reads.
type Kind string

const (
	Nodes        Kind = "nodes"
	Pods         Kind = "pods"
	Deployments  Kind = "deployments"
	StatefulSets Kind = "statefulsets"
	DaemonSets   Kind = "daemonsets"
	Jobs         Kind = "jobs"
	PVCs         Kind = "pvcs"
	Gateways     Kind = "gateways"
	HTTPRoutes   Kind = "httproutes"
)

// AlertKinds are the kinds checked for problems.
var AlertKinds = []Kind{Nodes, Pods, Deployments, StatefulSets, DaemonSets, Jobs, PVCs, Gateways, HTTPRoutes}

type Severity string

const (
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

type Source string

const (
	FromKubctl       Source = "kubctl"
	FromAlertmanager Source = "alertmanager"
)

// Alert is something wrong on a cluster, worded for a notification.
type Alert struct {
	// ID is stable across evaluations: "pods/shop/api-1", or "am:<fingerprint>".
	ID       string
	Title    string // "payments-api keeps crashing"
	Detail   string // "Deployment in payments · 2 of 3 ready · 2 pods crash-looping"
	Severity Severity
	Since    time.Time
	Source   Source
	// The resource to open when the alert is tapped, if any.
	Kind      Kind
	Namespace string
	Name      string
	// Affected lists pods folded into a workload's alert.
	Affected []string
}

type alertJSON struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Detail    string   `json:"detail"`
	Severity  Severity `json:"severity"`
	Since     string   `json:"since"`
	Source    Source   `json:"source"`
	Kind      Kind     `json:"kind,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	Name      string   `json:"name,omitempty"`
	Affected  []string `json:"affected,omitempty"`
}

// timeFormat is what Dart's toIso8601String writes for UTC times.
const timeFormat = "2006-01-02T15:04:05.000000Z"

func (a Alert) MarshalJSON() ([]byte, error) {
	src := a.Source
	if src == "" {
		src = FromKubctl
	}
	return json.Marshal(alertJSON{
		ID: a.ID, Title: a.Title, Detail: a.Detail, Severity: a.Severity,
		Since: a.Since.UTC().Format(timeFormat), Source: src,
		Kind: a.Kind, Namespace: a.Namespace, Name: a.Name, Affected: a.Affected,
	})
}

func (a *Alert) UnmarshalJSON(b []byte) error {
	var j alertJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	since, err := time.Parse(time.RFC3339Nano, j.Since)
	if err != nil {
		since = time.Now()
	}
	if j.Severity != Critical {
		j.Severity = Warning
	}
	if j.Source != FromAlertmanager {
		j.Source = FromKubctl
	}
	*a = Alert{
		ID: j.ID, Title: j.Title, Detail: j.Detail, Severity: j.Severity, Since: since, Source: j.Source,
		Kind: j.Kind, Namespace: j.Namespace, Name: j.Name, Affected: j.Affected,
	}
	return nil
}

// Changes is what one evaluation changed.
type Changes struct {
	Fired    []Alert
	Resolved []Alert
	// Updated are still firing, but worded differently (more pods affected…).
	Updated []Alert
}

func (c Changes) Empty() bool {
	return len(c.Fired) == 0 && len(c.Resolved) == 0 && len(c.Updated) == 0
}
