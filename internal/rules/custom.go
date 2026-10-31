package rules

import (
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Argo CD Applications and cert-manager Certificates come from CRDs whose Go
// types would pull in their whole projects, so they're read unstructured:
// only the few status fields below matter.

// CertificateNotice is how long before a certificate expires it alerts.
// cert-manager renews well before this (a third of the lifetime, 30 days
// for Let's Encrypt), so reaching it means renewal isn't happening.
const CertificateNotice = 14 * 24 * time.Hour

// applicationRow: Degraded health, or a sync that failed and left the app
// out of sync, is an error; healthy and synced again clears it.
func applicationRow(u *unstructured.Unstructured) Row {
	health := str(u.Object, "status", "health", "status")
	sync := str(u.Object, "status", "sync", "status")
	phase := str(u.Object, "status", "operationState", "phase")
	r := Row{Kind: Applications, Name: u.GetName(), Namespace: u.GetNamespace(), Labels: u.GetLabels(),
		Target: str(u.Object, "spec", "destination", "namespace"), Health: OK, Status: health}
	switch {
	case health == "Degraded":
		r.Health = Error
		r.Message = first(degradedResource(u), str(u.Object, "status", "health", "message"), conditionError(u))
	case (phase == "Failed" || phase == "Error") && sync != "Synced":
		r.Health, r.Status = Error, "SyncFailed"
		r.Message = first(str(u.Object, "status", "operationState", "message"), conditionError(u))
	case health == "Progressing" || sync == "OutOfSync":
		r.Health = Pending
	}
	r.Message = short(r.Message)
	return r
}

// degradedResource names the first unhealthy resource: "Deployment
// fraud-detector: exceeded its progress deadline".
func degradedResource(u *unstructured.Unstructured) string {
	resources, _, _ := unstructured.NestedSlice(u.Object, "status", "resources")
	for _, item := range resources {
		res, ok := item.(map[string]any)
		if !ok || str(res, "health", "status") != "Degraded" {
			continue
		}
		what := strings.TrimSpace(str(res, "kind") + " " + str(res, "name"))
		if msg := str(res, "health", "message"); msg != "" {
			return what + ": " + msg
		}
		return what + " is degraded"
	}
	return ""
}

// conditionError is the message of Argo CD's first error condition
// (ComparisonError, SyncError, InvalidSpecError…).
func conditionError(u *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, item := range conditions {
		if c, ok := item.(map[string]any); ok && strings.HasSuffix(str(c, "type"), "Error") {
			return str(c, "message")
		}
	}
	return ""
}

// certificateRow: not ready after a failed issuance is an error (a new
// certificate still being issued is only pending). How close expiry is
// depends on the time, so ProblemOf judges that from Expires.
func certificateRow(u *unstructured.Unstructured) Row {
	r := Row{Kind: Certificates, Name: u.GetName(), Namespace: u.GetNamespace(), Labels: u.GetLabels(),
		Health: OK, Status: "Ready"}
	if t, err := time.Parse(time.RFC3339, str(u.Object, "status", "notAfter")); err == nil {
		r.Expires = t
	}
	ready, readyMsg, _ := conditionOf(u, "Ready")
	_, issuingMsg, issuingReason := conditionOf(u, "Issuing")
	failed := issuingReason == "Failed" || str(u.Object, "status", "lastFailureTime") != ""
	switch {
	case ready == "True":
	case failed:
		r.Health, r.Status = Error, "Not ready"
		if issuingReason == "Failed" {
			r.Message = short(first(issuingMsg, readyMsg))
		} else {
			r.Message = short(first(readyMsg, issuingMsg))
		}
	default:
		r.Health, r.Status = Pending, "Not ready"
	}
	return r
}

func conditionOf(u *unstructured.Unstructured, kind string) (status, message, reason string) {
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, item := range conditions {
		if c, ok := item.(map[string]any); ok && str(c, "type") == kind {
			return str(c, "status"), str(c, "message"), str(c, "reason")
		}
	}
	return "", "", ""
}

// expiresIn: "expires in 6 days".
func expiresIn(left time.Duration) string {
	switch days := int(left / (24 * time.Hour)); days {
	case 0:
		return "expires in less than a day"
	case 1:
		return "expires in 1 day"
	default:
		return "expires in " + strconv.Itoa(days) + " days"
	}
}

func str(obj map[string]any, fields ...string) string {
	s, _, _ := unstructured.NestedString(obj, fields...)
	return strings.TrimSpace(s)
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// short keeps a controller's message to one line a notification can show.
func short(msg string) string {
	msg, _, _ = strings.Cut(strings.TrimSpace(msg), "\n")
	const most = 120
	if r := []rune(msg); len(r) > most {
		return strings.TrimSpace(string(r[:most-1])) + "…"
	}
	return msg
}
