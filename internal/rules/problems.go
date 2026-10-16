package rules

import (
	"fmt"
	"strings"
	"time"
)

// AlertID is "<kind>/<namespace or ->/<name>".
func AlertID(kind Kind, namespace, name string) string {
	if namespace == "" {
		namespace = "-"
	}
	return fmt.Sprintf("%s/%s/%s", kind, namespace, name)
}

// ProblemOf is the problem with one resource, if it has one worth alerting
// on. Pods are judged alone here; Problems folds them into their workload.
func ProblemOf(r Row, now time.Time) (Alert, bool) {
	var sev Severity
	var title string
	switch r.Kind {
	case Pods:
		switch r.Health {
		case Error:
			sev, title = Critical, r.Name+" "+PodTrouble(r.Status)
		case Pending:
			sev, title = Warning, r.Name+" "+PodTrouble(r.Status)
		}
	case Deployments, StatefulSets, DaemonSets:
		switch r.Health {
		case Error:
			state := "degraded"
			if r.Ready == 0 {
				state = "down"
			}
			sev, title = Critical, r.Name+" is "+state
		case Pending:
			sev, title = Warning, r.Name+" isn’t fully ready"
		}
	case Jobs:
		if r.Health == Error {
			sev, title = Critical, "Job "+r.Name+" failed"
		}
	case Nodes:
		if r.Health == Error {
			sev, title = Critical, "Node "+r.Name+" is not ready"
		}
	case PVCs:
		switch r.Health {
		case Error:
			sev, title = Critical, "Volume claim "+r.Name+" is "+strings.ToLower(r.Status)
		case Pending:
			sev, title = Warning, "Volume claim "+r.Name+" is stuck pending"
		}
	// Waiting for a controller is setup, not an incident: only alert on failures.
	case Gateways:
		if r.Health == Error {
			sev, title = Critical, "Gateway "+r.Name+" isn’t programmed"
		}
	case HTTPRoutes:
		if r.Health == Error {
			sev, title = Critical, "Route "+r.Name+" was rejected"
		}
	}
	if sev == "" {
		return Alert{}, false
	}
	return Alert{
		ID: AlertID(r.Kind, r.Namespace, r.Name), Title: title, Detail: detail(r), Severity: sev, Since: now,
		Source: FromClusterctl, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name,
	}, true
}

var workloadKinds = map[Kind]bool{Deployments: true, StatefulSets: true, DaemonSets: true, Jobs: true}

// Problems lists every current problem in rows. Pods that belong to a
// failing workload are folded into the workload's alert instead of
// alerting alone. Order: other kinds as given, then lone pods.
func Problems(rows []Row, now time.Time) []Alert {
	var order []string
	out := map[string]Alert{}
	add := func(a Alert) {
		if _, ok := out[a.ID]; !ok {
			order = append(order, a.ID)
		}
		out[a.ID] = a
	}
	type podAlert struct {
		row   Row
		alert Alert
	}
	var pods []podAlert
	for _, r := range rows {
		a, ok := ProblemOf(r, now)
		if !ok {
			continue
		}
		if r.Kind == Pods {
			pods = append(pods, podAlert{r, a})
		} else {
			add(a)
		}
	}
	folded := map[int]bool{}
	for _, w := range rows {
		if !workloadKinds[w.Kind] || len(w.Selector) == 0 {
			continue
		}
		id := AlertID(w.Kind, w.Namespace, w.Name)
		alert, failing := out[id]
		var owned []int
		for i, p := range pods {
			if p.row.Namespace == w.Namespace && matches(w.Selector, p.row.Labels) {
				owned = append(owned, i)
			}
		}
		// A healthy workload with a failing pod (say, a stuck rollout) lets the pod alert alone.
		if len(owned) == 0 || !failing {
			continue
		}
		worst := pods[owned[0]].row
		for _, i := range owned {
			if pods[i].row.Health == Error {
				worst = pods[i].row
				break
			}
		}
		for _, i := range owned {
			folded[i] = true
			alert.Affected = append(alert.Affected, pods[i].row.Name)
		}
		if alert.Severity == Critical && worst.Health == Error {
			alert.Title = w.Name + " " + PodTrouble(worst.Status)
		}
		noun := "pods"
		if len(owned) == 1 {
			noun = "pod"
		}
		alert.Detail = fmt.Sprintf("%s · %d %s %s", alert.Detail, len(owned), noun, plainStatus(worst.Status))
		out[id] = alert
	}
	for i, p := range pods {
		if !folded[i] {
			add(p.alert)
		}
	}
	list := make([]Alert, 0, len(order))
	for _, id := range order {
		list = append(list, out[id])
	}
	return list
}

// PodTrouble is a pod status in words a notification can use.
func PodTrouble(status string) string {
	switch status {
	case "CrashLoopBackOff", "Error", "Restarting":
		return "keeps crashing"
	case "OOMKilled":
		return "ran out of memory"
	case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
		return "can’t pull its image"
	case "CreateContainerConfigError", "CreateContainerError":
		return "has a configuration error"
	case "Pending":
		return "can’t be scheduled"
	case "ContainerCreating", "PodInitializing":
		return "is stuck starting"
	case "Evicted":
		return "was evicted"
	case "Running":
		return "isn’t ready"
	}
	if strings.HasPrefix(status, "Init:") {
		if strings.Contains(status, "Err") || strings.Contains(status, "BackOff") || strings.Contains(status, "ExitCode") {
			return "has a failing init container (" + status + ")"
		}
		return "is stuck starting (" + status + ")"
	}
	return "is unhealthy (" + status + ")"
}

func plainStatus(status string) string {
	switch status {
	case "CrashLoopBackOff", "Error", "Restarting":
		return "crash-looping"
	case "ImagePullBackOff", "ErrImagePull":
		return "can’t pull the image"
	case "OOMKilled":
		return "out of memory"
	case "Running":
		return "not ready"
	}
	return status
}

var singular = map[Kind]string{
	Deployments: "Deployment", StatefulSets: "StatefulSet", DaemonSets: "DaemonSet", Jobs: "Job",
	PVCs: "Volume claim", Gateways: "Gateway", HTTPRoutes: "HTTPRoute", Pods: "Pod", Nodes: "Node",
}

func detail(r Row) string {
	where := ""
	if r.Namespace != "" {
		where = " in " + r.Namespace
	}
	switch r.Kind {
	case Pods:
		restarts := ""
		if r.Restarts > 0 {
			restarts = fmt.Sprintf(" · %d restarts", r.Restarts)
		}
		return "Pod" + where + " · " + r.Status + restarts
	case Deployments, StatefulSets, DaemonSets:
		return fmt.Sprintf("%s%s · %d of %d ready", singular[r.Kind], where, r.Ready, r.Desired)
	case Nodes:
		return "Node · " + r.Status
	}
	return singular[r.Kind] + where + " · " + r.Status
}
