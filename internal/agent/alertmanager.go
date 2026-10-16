package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/prometheus/alertmanager/template"

	"github.com/RazaLabs/clusterctl-agent/internal/rules"
)

// AmAlert is one alert from an Alertmanager webhook, and whether it's firing.
type AmAlert struct {
	Alert  rules.Alert
	Firing bool
}

// FromAlertmanager reads Alertmanager's webhook payload.
func FromAlertmanager(data template.Data) []AmAlert {
	out := make([]AmAlert, 0, len(data.Alerts))
	for _, a := range data.Alerts {
		name := a.Labels["alertname"]
		if name == "" {
			name = "Alert"
		}
		fingerprint := a.Fingerprint
		if fingerprint == "" {
			fingerprint = fingerprintOf(a.Labels)
		}
		ns := a.Labels["namespace"]
		target := first(a.Labels["pod"], a.Labels["deployment"], a.Labels["statefulset"], a.Labels["node"], a.Labels["instance"])
		var where []string
		for _, w := range []string{ns, target} {
			if w != "" {
				where = append(where, w)
			}
		}
		detail := "Alertmanager · " + name
		if len(where) > 0 {
			detail += " · " + strings.Join(where, " / ")
		}
		title := first(a.Annotations["summary"], a.Annotations["message"], a.Annotations["description"], name)
		sev := rules.Warning
		if strings.EqualFold(a.Labels["severity"], "critical") {
			sev = rules.Critical
		}
		since := a.StartsAt
		if since.IsZero() {
			since = time.Now()
		}
		out = append(out, AmAlert{
			Alert: rules.Alert{
				ID: "am:" + fingerprint, Title: title, Detail: detail, Severity: sev, Since: since,
				Source: rules.FromAlertmanager, Namespace: ns,
			},
			Firing: a.Status != "resolved",
		})
	}
	return out
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// fingerprintOf stands in for Alertmanager's fingerprint when a sender
// leaves it out: stable for the same labels.
func fingerprintOf(labels template.KV) string {
	sum := sha256.New()
	for _, p := range labels.SortedPairs() {
		sum.Write([]byte(p.Name + "=" + p.Value + "\x00"))
	}
	return hex.EncodeToString(sum.Sum(nil))[:16]
}
