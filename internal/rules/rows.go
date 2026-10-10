package rules

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Health is how a resource is doing, as the app colors it.
type Health int

const (
	OK Health = iota
	Pending
	Error
	Completed
	Terminating
)

// Row is one resource reduced to what the rules look at.
type Row struct {
	Kind      Kind
	Name      string
	Namespace string
	Health    Health
	// Status is the app's status column: "CrashLoopBackOff", "Rejected", "NotReady"…
	Status   string
	Labels   map[string]string
	Selector map[string]string // workloads: which pods are theirs
	Ready    int               // workloads
	Desired  int               // workloads
	Restarts int               // pods
}

// Objects is a snapshot of the watched kinds. Nil slices are kinds that
// aren't loaded (not served, or not allowed).
type Objects struct {
	Pods         []*corev1.Pod
	Nodes        []*corev1.Node
	PVCs         []*corev1.PersistentVolumeClaim
	Deployments  []*appsv1.Deployment
	StatefulSets []*appsv1.StatefulSet
	DaemonSets   []*appsv1.DaemonSet
	Jobs         []*batchv1.Job
	Gateways     []*gatewayv1.Gateway
	HTTPRoutes   []*gatewayv1.HTTPRoute
}

// Rows turns objects into rows. Workload health needs the pods, so it
// should be given all pods in scope.
func Rows(o Objects) []Row {
	var rows []Row
	for _, p := range o.Pods {
		s := PodStatus(p)
		rows = append(rows, Row{Kind: Pods, Name: p.Name, Namespace: p.Namespace, Health: s.Health,
			Status: s.Reason, Labels: p.Labels, Restarts: s.Restarts})
	}
	workload := func(kind Kind, m *metav1.ObjectMeta, desired, ready int, sel *metav1.LabelSelector) Row {
		selector := map[string]string{}
		if sel != nil {
			selector = sel.MatchLabels
		}
		return Row{Kind: kind, Name: m.Name, Namespace: m.Namespace, Labels: m.Labels, Selector: selector,
			Ready: ready, Desired: desired, Status: fmt.Sprintf("%d/%d", ready, desired),
			Health: workloadHealth(desired, ready, m.Namespace, selector, o.Pods)}
	}
	for _, d := range o.Deployments {
		rows = append(rows, workload(Deployments, &d.ObjectMeta, replicas(d.Spec.Replicas), int(d.Status.ReadyReplicas), d.Spec.Selector))
	}
	for _, s := range o.StatefulSets {
		rows = append(rows, workload(StatefulSets, &s.ObjectMeta, replicas(s.Spec.Replicas), int(s.Status.ReadyReplicas), s.Spec.Selector))
	}
	for _, d := range o.DaemonSets {
		r := workload(DaemonSets, &d.ObjectMeta, int(d.Status.DesiredNumberScheduled), int(d.Status.NumberReady), d.Spec.Selector)
		r.Status = fmt.Sprint(d.Status.NumberReady) // the app's READY column
		rows = append(rows, r)
	}
	for _, j := range o.Jobs {
		completions := 1
		if j.Spec.Completions != nil {
			completions = int(*j.Spec.Completions)
		}
		health := Pending
		if jobCondition(j, batchv1.JobComplete) {
			health = Completed
		} else if jobCondition(j, batchv1.JobFailed) {
			health = Error
		}
		selector := map[string]string{}
		if j.Spec.Selector != nil {
			selector = j.Spec.Selector.MatchLabels
		}
		rows = append(rows, Row{Kind: Jobs, Name: j.Name, Namespace: j.Namespace, Labels: j.Labels, Selector: selector,
			Health: health, Status: fmt.Sprintf("%d/%d", j.Status.Succeeded, completions)})
	}
	for _, c := range o.PVCs {
		health := Error
		switch c.Status.Phase {
		case corev1.ClaimBound:
			health = OK
		case corev1.ClaimPending:
			health = Pending
		}
		rows = append(rows, Row{Kind: PVCs, Name: c.Name, Namespace: c.Namespace, Labels: c.Labels,
			Health: health, Status: string(c.Status.Phase)})
	}
	for _, n := range o.Nodes {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		status := "NotReady"
		if ready {
			status = "Ready"
		}
		if n.Spec.Unschedulable {
			status += ",SchedulingDisabled"
		}
		health := Error
		if ready {
			health = OK
		}
		rows = append(rows, Row{Kind: Nodes, Name: n.Name, Labels: n.Labels, Health: health, Status: status})
	}
	for _, g := range o.Gateways {
		status, text := "", "Waiting for controller"
		if c := apimeta.FindStatusCondition(g.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed)); c != nil {
			status = string(c.Status)
		}
		switch status {
		case "True":
			text = "Programmed"
		case "False":
			text = "Not programmed"
		}
		rows = append(rows, Row{Kind: Gateways, Name: g.Name, Namespace: g.Namespace, Labels: g.Labels,
			Health: conditionHealth(status), Status: text})
	}
	for _, r := range o.HTTPRoutes {
		status := routeAccepted(r)
		text := "Waiting for gateway"
		switch status {
		case "True":
			text = "Accepted"
		case "False":
			text = "Rejected"
		}
		rows = append(rows, Row{Kind: HTTPRoutes, Name: r.Name, Namespace: r.Namespace, Labels: r.Labels,
			Health: conditionHealth(status), Status: text})
	}
	return rows
}

func replicas(r *int32) int {
	if r == nil {
		return 1
	}
	return int(*r)
}

func jobCondition(j *batchv1.Job, t batchv1.JobConditionType) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func conditionHealth(status string) Health {
	switch status {
	case "True":
		return OK
	case "False":
		return Error
	}
	return Pending
}

// routeAccepted sums up every parent's conditions: any False rejects the
// route, all True accepts it, no conditions means no gateway answered yet.
func routeAccepted(r *gatewayv1.HTTPRoute) string {
	var conds []string
	for _, p := range r.Status.Parents {
		for _, c := range p.Conditions {
			conds = append(conds, string(c.Status))
		}
	}
	if len(conds) == 0 {
		return ""
	}
	all := true
	for _, c := range conds {
		if c == "False" {
			return "False"
		}
		all = all && c == "True"
	}
	if all {
		return "True"
	}
	return "Unknown"
}

// workloadHealth: scaled to zero is done, all ready is fine; otherwise it's
// failing if any of its pods is, else still rolling out.
func workloadHealth(desired, ready int, ns string, selector map[string]string, pods []*corev1.Pod) Health {
	if desired == 0 {
		return Completed
	}
	if ready >= desired {
		return OK
	}
	if len(selector) > 0 {
		for _, p := range pods {
			if p.Namespace == ns && matches(selector, p.Labels) && PodStatus(p).Health == Error {
				return Error
			}
		}
	}
	return Pending
}

func matches(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// PodStatusResult is kubectl's STATUS, READY and RESTARTS, plus our health.
type PodStatusResult struct {
	Reason   string
	Ready    string
	Restarts int
	Health   Health
}

// PodStatus is kubectl's pod STATUS column logic.
func PodStatus(pod *corev1.Pod) PodStatusResult {
	st, spec := pod.Status, pod.Spec
	total := len(spec.Containers)
	reason := st.Reason
	if reason == "" {
		reason = string(st.Phase)
	}
	restarts, readyCount := 0, 0

	// Init containers first: a pod stuck initializing reports that.
	initializing := false
	for i, c := range st.InitContainerStatuses {
		restarts += int(c.RestartCount)
		term, wait := c.State.Terminated, c.State.Waiting
		if term != nil && term.ExitCode == 0 {
			continue
		}
		initializing = true
		switch {
		case term != nil && term.Reason != "":
			reason = "Init:" + term.Reason
		case term != nil:
			reason = fmt.Sprintf("Init:ExitCode:%d", term.ExitCode)
		case wait != nil && wait.Reason != "" && wait.Reason != "PodInitializing":
			reason = "Init:" + wait.Reason
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(spec.InitContainers))
		}
		break
	}

	if !initializing {
		hasRunning := false
		for i := len(st.ContainerStatuses) - 1; i >= 0; i-- {
			c := st.ContainerStatuses[i]
			restarts += int(c.RestartCount)
			switch s := c.State; {
			case s.Waiting != nil && s.Waiting.Reason != "":
				reason = s.Waiting.Reason
			case s.Terminated != nil && s.Terminated.Reason != "":
				reason = s.Terminated.Reason
			case s.Terminated != nil && s.Terminated.Signal != 0:
				reason = fmt.Sprintf("Signal:%d", s.Terminated.Signal)
			case s.Terminated != nil:
				reason = fmt.Sprintf("ExitCode:%d", s.Terminated.ExitCode)
			case s.Running != nil && c.Ready:
				hasRunning = true
				readyCount++
			}
		}
		if reason == "Completed" && hasRunning {
			reason = "Running"
		}
	}

	deleting := pod.DeletionTimestamp != nil
	if deleting {
		reason = "Terminating"
		if st.Reason == "NodeLost" {
			reason = "Unknown"
		}
	}

	var health Health
	switch {
	case deleting:
		health = Terminating
	case reason == "Running":
		switch {
		case readyCount == total:
			health = OK
		case restarts > 3:
			health = Error
		default:
			health = Pending
		}
	case reason == "Completed" || reason == "Succeeded":
		health = Completed
	case reason == "Pending" || reason == "ContainerCreating" || reason == "PodInitializing",
		strings.HasPrefix(reason, "Init:") && !strings.Contains(reason, "Err") && !strings.Contains(reason, "BackOff"):
		health = Pending
	default:
		health = Error
	}
	return PodStatusResult{Reason: reason, Ready: fmt.Sprintf("%d/%d", readyCount, total), Restarts: restarts, Health: health}
}
