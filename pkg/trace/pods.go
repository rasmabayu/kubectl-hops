package trace

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// maxPodChecks caps how many unhealthy pods are listed one by one.
	maxPodChecks = 5
	// recentRestart is how far back a container restart still counts as news.
	recentRestart = 24 * time.Hour
	maxDetailLen  = 200
)

// podDiagnosis is what is wrong with one pod, if anything.
type podDiagnosis struct {
	status  Status
	message string
	details []string
	cause   string
	try     []string
}

// podsHop diagnoses the pods behind a Service or Deployment: why each one is
// not ready, and whether ready ones keep restarting.
func (t *Tracer) podsHop(ctx context.Context, ns, name string, pods []corev1.Pod) (Hop, error) {
	hop := Hop{Kind: "Pods", Namespace: ns, Name: name}

	events, err := t.podEvents(ctx, ns)
	if err != nil {
		return hop, err
	}
	nodes := map[string]*corev1.Node{}

	sorted := append([]corev1.Pod(nil), pods...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	ready := 0
	var problems []Check
	for i := range sorted {
		p := &sorted[i]
		if podReady(p) {
			ready++
		}
		d, err := t.diagnosePod(ctx, p, events[p.Name], nodes)
		if err != nil {
			return hop, err
		}
		if d.status == StatusOK {
			continue
		}
		problems = append(problems, Check{
			Name:    "pod " + p.Name,
			Status:  d.status,
			Message: d.message,
			Details: d.details,
			Cause:   d.cause,
			Try:     d.try,
		})
	}

	// While some pods are ready, traffic still flows, so a broken pod only
	// degrades the path.
	if ready > 0 {
		for i := range problems {
			if problems[i].Status == StatusFail {
				problems[i].Status = StatusWarn
			}
		}
	}

	total := len(sorted)
	summary := Check{Name: "ready", Message: fmt.Sprintf("%d/%d pod(s) ready", ready, total)}
	switch {
	case ready == 0:
		summary.Status = StatusFail
	case ready < total:
		summary.Status = StatusWarn
	default:
		summary.Status = StatusOK
	}
	hop.Add(summary)

	// Failures first, so the cap never hides the pods that break traffic.
	sort.SliceStable(problems, func(i, j int) bool { return problems[i].Status > problems[j].Status })
	for i, c := range problems {
		if i == maxPodChecks {
			hop.Add(Check{Name: "more", Status: StatusInfo, Message: fmt.Sprintf("%d more pod(s) with problems not shown", len(problems)-maxPodChecks)})
			break
		}
		hop.Add(c)
	}
	return hop, nil
}

// podEvents lists the namespace's Pod events once, grouped by pod name.
// Missing permission to read events only loses detail.
func (t *Tracer) podEvents(ctx context.Context, ns string) (map[string][]corev1.Event, error) {
	list, err := t.Client.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.kind=Pod"})
	if apierrors.IsForbidden(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list events in %s: %w", ns, err)
	}
	byPod := map[string][]corev1.Event{}
	for _, e := range list.Items {
		if e.InvolvedObject.Kind == "Pod" {
			byPod[e.InvolvedObject.Name] = append(byPod[e.InvolvedObject.Name], e)
		}
	}
	return byPod, nil
}

// diagnosePod works out the most important problem with a pod, in the order a
// human would check: scheduling, image, config, crashes, node, readiness, and
// finally recent restarts of an otherwise healthy pod.
func (t *Tracer) diagnosePod(ctx context.Context, p *corev1.Pod, events []corev1.Event, nodes map[string]*corev1.Node) (podDiagnosis, error) {
	ns := p.Namespace
	describe := fmt.Sprintf("kubectl describe pod %s -n %s", p.Name, ns)
	eventsCmd := fmt.Sprintf("kubectl get events -n %s --field-selector involvedObject.name=%s", ns, p.Name)

	if p.DeletionTimestamp != nil {
		return podDiagnosis{status: StatusInfo, message: "terminating"}, nil
	}

	if c := podCondition(p, corev1.PodScheduled); c != nil && c.Status == corev1.ConditionFalse {
		return podDiagnosis{
			status:  StatusFail,
			message: "Pending: " + firstNonEmpty(c.Reason, "not scheduled"),
			details: nonEmpty(truncate(c.Message)),
			cause:   "The scheduler cannot place the pod: not enough CPU or memory, node selectors or affinity that match no node, untolerated taints, or an unbound PersistentVolumeClaim.",
			try:     []string{describe, "kubectl get nodes"},
		}, nil
	}

	for _, cs := range allContainerStatuses(p) {
		w := cs.State.Waiting
		if w == nil {
			continue
		}
		switch w.Reason {
		case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "ErrImageNeverPull":
			return podDiagnosis{
				status:  StatusFail,
				message: fmt.Sprintf("container %s: %s (%s)", cs.Name, w.Reason, cs.Image),
				details: nonEmpty(truncate(w.Message)),
				cause:   "The image cannot be pulled: a wrong name or tag, a private registry without imagePullSecrets, or a registry that cannot be reached from the nodes.",
				try:     []string{describe},
			}, nil
		case "CreateContainerConfigError", "CreateContainerError":
			return podDiagnosis{
				status:  StatusFail,
				message: fmt.Sprintf("container %s: %s", cs.Name, w.Reason),
				details: nonEmpty(truncate(w.Message)),
				cause:   "The container cannot be created, usually because a referenced ConfigMap, Secret or key does not exist.",
				try:     []string{describe},
			}, nil
		case "CrashLoopBackOff":
			d := podDiagnosis{
				status:  StatusFail,
				message: fmt.Sprintf("container %s: CrashLoopBackOff, %d restart(s)", cs.Name, cs.RestartCount),
				try:     []string{fmt.Sprintf("kubectl logs %s -n %s -c %s --previous", p.Name, ns, cs.Name), describe},
				cause:   "The container keeps exiting right after it starts; its previous logs usually say why (bad config, missing dependency, failed migration).",
			}
			if term := cs.LastTerminationState.Terminated; term != nil {
				d.details = []string{fmt.Sprintf("last exit: %s, code %d", firstNonEmpty(term.Reason, "Error"), term.ExitCode)}
				if term.Reason == "OOMKilled" {
					d.cause = "The container is killed for exceeding its memory limit. Raise resources.limits.memory or find what uses the memory."
				}
			}
			return d, nil
		}
	}

	if p.Spec.NodeName != "" {
		node, err := t.node(ctx, p.Spec.NodeName, nodes)
		if err != nil {
			return podDiagnosis{}, err
		}
		if node != nil {
			if c := nodeReadyCondition(node); c == nil || c.Status != corev1.ConditionTrue {
				reason := "unknown"
				if c != nil {
					reason = firstNonEmpty(c.Reason, string(c.Status))
				}
				return podDiagnosis{
					status:  StatusFail,
					message: fmt.Sprintf("node %s is NotReady (%s)", node.Name, reason),
					cause:   "The pod's node is unhealthy, so its pods are marked not ready and taken out of the Service until the node recovers or the pods are rescheduled.",
					try:     []string{fmt.Sprintf("kubectl describe node %s", node.Name)},
				}, nil
			}
		}
	}

	if !podReady(p) {
		if p.Status.Phase == corev1.PodPending {
			return podDiagnosis{
				status:  StatusWarn,
				message: "Pending: " + firstNonEmpty(waitingReason(p), "starting"),
				cause:   "The pod is still starting (pulling images or mounting volumes).",
				try:     []string{describe},
			}, nil
		}
		if e := latestEvent(events, "Unhealthy", "Readiness probe failed"); e != nil {
			return podDiagnosis{
				status:  StatusFail,
				message: "readiness probe failing",
				details: []string{truncate(e.Message) + countSuffix(e.Count)},
				cause:   "The container runs but its readiness probe fails: the probe path or port is wrong, a dependency it checks is down, or the app needs longer to start than the probe allows.",
				try:     []string{describe, fmt.Sprintf("kubectl logs %s -n %s", p.Name, ns)},
			}, nil
		}
		return podDiagnosis{
			status:  StatusFail,
			message: "not ready: " + strings.Join(notReadyContainers(p), ", "),
			cause:   "The pod is running but not ready and no probe failure events were found; the events may have expired, or a readiness gate is not satisfied.",
			try:     []string{describe, eventsCmd},
		}, nil
	}

	return t.recentRestarts(p, events, describe), nil
}

// recentRestarts flags a ready pod whose containers restarted recently.
func (t *Tracer) recentRestarts(p *corev1.Pod, events []corev1.Event, describe string) podDiagnosis {
	now := t.now()
	var parts, details []string
	oom := false
	for _, cs := range allContainerStatuses(p) {
		term := cs.LastTerminationState.Terminated
		if cs.RestartCount == 0 || term == nil || now.Sub(term.FinishedAt.Time) > recentRestart {
			continue
		}
		parts = append(parts, fmt.Sprintf("container %s restarted %d time(s), last %s ago", cs.Name, cs.RestartCount, shortDuration(now.Sub(term.FinishedAt.Time))))
		details = append(details, fmt.Sprintf("%s last exit: %s, code %d", cs.Name, firstNonEmpty(term.Reason, "Error"), term.ExitCode))
		if term.Reason == "OOMKilled" {
			oom = true
		}
	}
	if len(parts) == 0 {
		return podDiagnosis{status: StatusOK}
	}
	if e := latestEvent(events, "Unhealthy", "Liveness probe failed"); e != nil {
		details = append(details, truncate(e.Message)+countSuffix(e.Count))
	}
	cause := "The pod is serving but restarted in the last 24h; each restart drops in-flight requests."
	if oom {
		cause = "The pod restarted after being OOMKilled; it is close to its memory limit and will likely be killed again."
	}
	return podDiagnosis{
		status:  StatusWarn,
		message: strings.Join(parts, "; "),
		details: details,
		cause:   cause,
		try:     []string{fmt.Sprintf("kubectl logs %s -n %s --previous", p.Name, p.Namespace), describe},
	}
}

// node fetches and caches a node; nil means it could not be read.
func (t *Tracer) node(ctx context.Context, name string, cache map[string]*corev1.Node) (*corev1.Node, error) {
	if n, ok := cache[name]; ok {
		return n, nil
	}
	n, err := t.Client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsForbidden(err) || apierrors.IsNotFound(err) {
		cache[name] = nil
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get node %s: %w", name, err)
	}
	cache[name] = n
	return n, nil
}

func podCondition(p *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == t {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

func nodeReadyCondition(n *corev1.Node) *corev1.NodeCondition {
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == corev1.NodeReady {
			return &n.Status.Conditions[i]
		}
	}
	return nil
}

func allContainerStatuses(p *corev1.Pod) []corev1.ContainerStatus {
	return append(append([]corev1.ContainerStatus(nil), p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
}

func waitingReason(p *corev1.Pod) string {
	for _, cs := range allContainerStatuses(p) {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}
	}
	return ""
}

func notReadyContainers(p *corev1.Pod) []string {
	var out []string
	for _, cs := range p.Status.ContainerStatuses {
		if !cs.Ready {
			out = append(out, "container "+cs.Name)
		}
	}
	if len(out) == 0 {
		return []string{"Ready condition is false"}
	}
	return out
}

// latestEvent returns the most recent event with the given reason whose
// message starts with prefix.
func latestEvent(events []corev1.Event, reason, prefix string) *corev1.Event {
	var best *corev1.Event
	for i := range events {
		e := &events[i]
		if e.Reason != reason || !strings.HasPrefix(e.Message, prefix) {
			continue
		}
		if best == nil || eventTime(e).After(eventTime(best)) {
			best = e
		}
	}
	return best
}

func eventTime(e *corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	default:
		return e.FirstTimestamp.Time
	}
}

func countSuffix(n int32) string {
	if n > 1 {
		return fmt.Sprintf(" (x%d)", n)
	}
	return ""
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func truncate(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxDetailLen {
		return string(r[:maxDetailLen-1]) + "…"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}
