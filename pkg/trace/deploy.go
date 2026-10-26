package trace

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// traceDeployment returns the Deployment hop followed by the hops of every
// Service that selects its pods. Without such a Service it still diagnoses
// the pods directly.
func (t *Tracer) traceDeployment(ctx context.Context, ns, name string) ([]Hop, error) {
	hop := Hop{Kind: "Deployment", Namespace: ns, Name: name}

	d, err := t.Client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		hop.Add(Check{
			Name:    "exists",
			Status:  StatusFail,
			Message: fmt.Sprintf("Deployment %q not found in namespace %q", name, ns),
			Cause:   "The Deployment does not exist (wrong name or namespace, or it was never applied).",
			Try:     []string{fmt.Sprintf("kubectl get deploy -n %s", ns)},
		})
		return []Hop{hop}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get deployment %s/%s: %w", ns, name, err)
	}
	hop.Add(Check{Name: "exists", Status: StatusOK, Message: fmt.Sprintf("generation %d", d.Generation)})
	checkReplicas(d, &hop)
	checkRollout(d, &hop)

	services, err := t.servicesSelecting(ctx, ns, d.Spec.Template.Labels)
	if err != nil {
		return nil, err
	}
	if err := t.ingressesRoutingTo(ctx, ns, services, &hop); err != nil {
		return nil, err
	}

	if len(services) == 0 {
		hop.Add(Check{
			Name:    "services",
			Status:  StatusWarn,
			Message: "no Service selects this Deployment's pods",
			Cause:   "Nothing routes traffic to these pods: no Service selector matches the pod template labels.",
			Try: []string{
				fmt.Sprintf("kubectl get svc -n %s -o custom-columns=NAME:.metadata.name,SELECTOR:.spec.selector", ns),
				fmt.Sprintf("kubectl get deploy %s -n %s -o jsonpath='{.spec.template.metadata.labels}'", name, ns),
			},
		})
		pods, err := t.deploymentPods(ctx, d)
		if err != nil {
			return nil, err
		}
		if len(pods) == 0 {
			return []Hop{hop}, nil
		}
		podsHop, err := t.podsHop(ctx, ns, name, pods)
		if err != nil {
			return nil, err
		}
		adoptPodCause(&hop, "replicas", &podsHop)
		return []Hop{hop, podsHop}, nil
	}

	names := make([]string, len(services))
	for i, s := range services {
		names[i] = "svc/" + s.Name
	}
	hop.Add(Check{Name: "services", Status: StatusOK, Message: "selected by " + strings.Join(names, ", ")})

	// Every Service selects the same pods, so their per-Service Pods hops
	// would repeat; show the Deployment's own pods once, at the end.
	hops := []Hop{hop}
	for _, s := range services {
		svcHops, err := t.traceService(ctx, ns, s.Name)
		if err != nil {
			return nil, err
		}
		for _, h := range svcHops {
			if h.Kind != "Pods" {
				hops = append(hops, h)
			}
		}
	}
	pods, err := t.deploymentPods(ctx, d)
	if err != nil {
		return nil, err
	}
	if len(pods) > 0 {
		podsHop, err := t.podsHop(ctx, ns, name, pods)
		if err != nil {
			return nil, err
		}
		adoptPodCause(&hops[0], "replicas", &podsHop)
		hops = append(hops, podsHop)
	}
	return hops, nil
}

func checkReplicas(d *appsv1.Deployment, hop *Hop) {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	ready := d.Status.ReadyReplicas
	try := []string{fmt.Sprintf("kubectl get pods -n %s -l %s", d.Namespace, selectorString(d))}
	switch {
	case want == 0:
		hop.Add(Check{
			Name:    "replicas",
			Status:  StatusWarn,
			Message: "scaled to 0 replicas",
			Cause:   "The Deployment is scaled to zero, so there are no pods to serve traffic.",
			Try:     []string{fmt.Sprintf("kubectl get hpa -n %s", d.Namespace)},
		})
	case ready == 0:
		hop.Add(Check{
			Name:    "replicas",
			Status:  StatusFail,
			Message: fmt.Sprintf("0/%d replicas ready", want),
			Cause:   "None of the Deployment's pods are ready; the Pods hop shows why.",
			Try:     try,
		})
	case ready < want:
		hop.Add(Check{
			Name:    "replicas",
			Status:  StatusWarn,
			Message: fmt.Sprintf("%d/%d replicas ready", ready, want),
			Cause:   "Some replicas are not ready; traffic flows with less capacity.",
			Try:     try,
		})
	default:
		hop.Add(Check{Name: "replicas", Status: StatusOK, Message: fmt.Sprintf("%d/%d replicas ready", ready, want)})
	}
}

func checkRollout(d *appsv1.Deployment, hop *Hop) {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
			hop.Add(Check{
				Name:    "rollout",
				Status:  StatusFail,
				Message: "rollout stuck: progress deadline exceeded",
				Details: nonEmpty(truncate(c.Message)),
				Cause:   "The latest rollout stopped making progress, usually because the new pods never become ready.",
				Try: []string{
					fmt.Sprintf("kubectl rollout status deploy/%s -n %s", d.Name, d.Namespace),
					fmt.Sprintf("kubectl rollout history deploy/%s -n %s", d.Name, d.Namespace),
				},
			})
			return
		}
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	if d.Status.ObservedGeneration < d.Generation || d.Status.UpdatedReplicas < want {
		hop.Add(Check{
			Name:    "rollout",
			Status:  StatusInfo,
			Message: fmt.Sprintf("rollout in progress: %d/%d replicas updated", d.Status.UpdatedReplicas, want),
		})
	}
}

func selectorString(d *appsv1.Deployment) string {
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return labels.SelectorFromSet(d.Spec.Template.Labels).String()
	}
	return sel.String()
}

// servicesSelecting returns the Services in ns whose selector matches podLabels.
func (t *Tracer) servicesSelecting(ctx context.Context, ns string, podLabels map[string]string) ([]corev1.Service, error) {
	list, err := t.Client.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list services in %s: %w", ns, err)
	}
	var out []corev1.Service
	for _, s := range list.Items {
		if len(s.Spec.Selector) == 0 || s.Spec.Type == corev1.ServiceTypeExternalName {
			continue
		}
		if labels.SelectorFromSet(s.Spec.Selector).Matches(labels.Set(podLabels)) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ingressesRoutingTo notes which Ingresses send traffic to these Services, so
// the user knows which entry point to trace next.
func (t *Tracer) ingressesRoutingTo(ctx context.Context, ns string, services []corev1.Service, hop *Hop) error {
	if len(services) == 0 {
		return nil
	}
	want := map[string]bool{}
	for _, s := range services {
		want[s.Name] = true
	}
	list, err := t.Client.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{})
	if apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list ingresses in %s: %w", ns, err)
	}
	seen := map[string]bool{}
	var routes []string
	for _, ing := range list.Items {
		if b := ing.Spec.DefaultBackend; b != nil && b.Service != nil && want[b.Service.Name] {
			routes = appendRoute(routes, seen, fmt.Sprintf("ingress/%s (default backend)", ing.Name))
		}
		for _, r := range ing.Spec.Rules {
			if r.HTTP == nil {
				continue
			}
			for _, p := range r.HTTP.Paths {
				if p.Backend.Service != nil && want[p.Backend.Service.Name] {
					routes = appendRoute(routes, seen, fmt.Sprintf("ingress/%s (%s%s)", ing.Name, displayHost(r.Host), p.Path))
				}
			}
		}
	}
	if len(routes) > 0 {
		hop.Add(Check{Name: "ingresses", Status: StatusInfo, Message: "exposed via " + strings.Join(routes, ", ")})
	}
	return nil
}

func appendRoute(routes []string, seen map[string]bool, r string) []string {
	if seen[r] {
		return routes
	}
	seen[r] = true
	return append(routes, r)
}

// deploymentPods lists the Deployment's live pods by its selector.
func (t *Tracer) deploymentPods(ctx context.Context, d *appsv1.Deployment) ([]corev1.Pod, error) {
	list, err := t.Client.CoreV1().Pods(d.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selectorString(d)})
	if err != nil {
		return nil, fmt.Errorf("list pods for deployment %s/%s: %w", d.Namespace, d.Name, err)
	}
	var pods []corev1.Pod
	for _, p := range list.Items {
		if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			pods = append(pods, p)
		}
	}
	return pods, nil
}
