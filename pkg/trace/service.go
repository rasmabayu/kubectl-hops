package trace

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// traceService returns the Service hop followed by its EndpointSlices hop.
func (t *Tracer) traceService(ctx context.Context, ns, name string) ([]Hop, error) {
	svcHop := Hop{Kind: "Service", Namespace: ns, Name: name}

	svc, err := t.Client.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		svcHop.Add(Check{
			Name:    "exists",
			Status:  StatusFail,
			Message: fmt.Sprintf("Service %q not found in namespace %q", name, ns),
			Cause:   "The Service does not exist (wrong name or namespace, or it was never applied), so nothing can route to it.",
			Try:     []string{fmt.Sprintf("kubectl get svc -n %s", ns)},
		})
		return []Hop{svcHop}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get service %s/%s: %w", ns, name, err)
	}
	svcHop.Add(Check{Name: "exists", Status: StatusOK, Message: describeService(svc)})

	if svc.Spec.Type == corev1.ServiceTypeExternalName {
		svcHop.Add(Check{
			Name:    "externalName",
			Status:  StatusInfo,
			Message: fmt.Sprintf("ExternalName → %s; traffic leaves the cluster via DNS, no endpoints or pods to trace", svc.Spec.ExternalName),
		})
		return []Hop{svcHop}, nil
	}
	if svc.Spec.ClusterIP == corev1.ClusterIPNone {
		svcHop.Add(Check{
			Name:    "headless",
			Status:  StatusInfo,
			Message: "headless (clusterIP: None): clients resolve pod IPs via DNS, no virtual IP load-balancing",
		})
	}

	pods, err := t.checkSelector(ctx, svc, &svcHop)
	if err != nil {
		return nil, err
	}
	if len(pods) > 0 {
		checkTargetPorts(svc, pods, &svcHop)
	}

	epHop, err := t.checkEndpointSlices(ctx, svc, len(pods))
	if err != nil {
		return nil, err
	}
	return []Hop{svcHop, epHop}, nil
}

func describeService(svc *corev1.Service) string {
	ports := make([]string, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		ports = append(ports, fmt.Sprintf("%d→%s", p.Port, portString(effectiveTargetPort(p))))
	}
	desc := string(svc.Spec.Type)
	if desc == "" {
		desc = string(corev1.ServiceTypeClusterIP)
	}
	if len(ports) > 0 {
		desc += ", ports " + strings.Join(ports, ", ")
	}
	return desc
}

// effectiveTargetPort applies the API default: an unset targetPort equals port.
func effectiveTargetPort(p corev1.ServicePort) intstr.IntOrString {
	if p.TargetPort.Type == intstr.String && p.TargetPort.StrVal != "" {
		return p.TargetPort
	}
	if p.TargetPort.Type == intstr.Int && p.TargetPort.IntVal != 0 {
		return p.TargetPort
	}
	return intstr.FromInt32(p.Port)
}

// checkSelector verifies the selector picks up pods and returns those pods.
func (t *Tracer) checkSelector(ctx context.Context, svc *corev1.Service, hop *Hop) ([]corev1.Pod, error) {
	ns := svc.Namespace
	if len(svc.Spec.Selector) == 0 {
		hop.Add(Check{
			Name:    "selector",
			Status:  StatusWarn,
			Message: "Service has no selector; its EndpointSlices must be managed by hand or by another controller",
			Cause:   "Without a selector Kubernetes never adds pods to this Service. That is intended for external backends, but a common mistake otherwise.",
			Try:     []string{fmt.Sprintf("kubectl get svc %s -n %s -o yaml", svc.Name, ns)},
		})
		return nil, nil
	}

	sel := labels.SelectorFromSet(svc.Spec.Selector)
	list, err := t.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", ns, err)
	}
	// Completed pods never become endpoints, so leave them out.
	var pods []corev1.Pod
	for _, p := range list.Items {
		if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			pods = append(pods, p)
		}
	}

	if len(pods) > 0 {
		hop.Add(Check{
			Name:    "selector",
			Status:  StatusOK,
			Message: fmt.Sprintf("selector %s matches %d pod(s)", sel, len(pods)),
		})
		return pods, nil
	}

	details, err := t.selectorBreakdown(ctx, ns, svc.Spec.Selector)
	if err != nil {
		return nil, err
	}
	hop.Add(Check{
		Name:    "selector",
		Status:  StatusFail,
		Message: fmt.Sprintf("selector %s matches 0 pods", sel),
		Details: details,
		Cause:   "The Service selector does not match the labels of any running pod, so the Service has no endpoints and every request fails (an Ingress in front of it typically returns 503).",
		Try: []string{
			fmt.Sprintf("kubectl get pods -n %s --show-labels", ns),
			fmt.Sprintf("kubectl get svc %s -n %s -o jsonpath='{.spec.selector}'", svc.Name, ns),
		},
	})
	return nil, nil
}

// selectorBreakdown explains, key by key, which part of a selector misses and
// which values of that label do exist in the namespace.
func (t *Tracer) selectorBreakdown(ctx context.Context, ns string, selector map[string]string) ([]string, error) {
	all, err := t.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", ns, err)
	}
	if len(all.Items) == 0 {
		return []string{fmt.Sprintf("namespace %s has no pods at all", ns)}, nil
	}

	keys := make([]string, 0, len(selector))
	for k := range selector {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []string
	for _, k := range keys {
		want := selector[k]
		matches := 0
		seen := map[string]bool{}
		for _, p := range all.Items {
			v, ok := p.Labels[k]
			if !ok {
				continue
			}
			if v == want {
				matches++
			} else {
				seen[v] = true
			}
		}
		line := fmt.Sprintf("%s=%s matches %d pod(s)", k, want, matches)
		if matches == 0 && len(seen) > 0 {
			line += "; found instead: " + joinSorted(seen, k)
		}
		out = append(out, line)
	}
	return out, nil
}

func joinSorted(set map[string]bool, key string) string {
	vals := make([]string, 0, len(set))
	for v := range set {
		vals = append(vals, key+"="+v)
	}
	sort.Strings(vals)
	return strings.Join(vals, ", ")
}

// checkTargetPorts compares each Service targetPort with the containerPorts
// of the selected pods.
//
// A named targetPort that no container declares is a hard failure: the
// EndpointSlice controller cannot resolve it and drops the pod. A numeric
// targetPort only needs the process to listen, since containerPorts are
// informational, but when pods declare ports and none of them match, a typo is
// far more likely than an undeclared listener, so it is reported as a failure
// with that caveat spelled out.
//
// A probe that targets the numeric port also proves something listens there,
// which covers the common case of pods that declare no containerPorts.
func checkTargetPorts(svc *corev1.Service, pods []corev1.Pod, hop *Hop) {
	for _, sp := range svc.Spec.Ports {
		tp := effectiveTargetPort(sp)
		proto := sp.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		portLabel := sp.Name
		if portLabel == "" {
			portLabel = fmt.Sprint(sp.Port)
		}
		name := "targetPort " + portLabel

		matched, viaProbe := 0, 0
		declared, probes := map[string]bool{}, map[string]bool{}
		for _, p := range pods {
			declaredHit := podHasPort(p, tp, proto, declared)
			probeHit := podProbesPort(p, tp, proto, probes)
			if declaredHit || probeHit {
				matched++
			}
			if probeHit && !declaredHit {
				viaProbe++
			}
		}

		switch {
		case matched == len(pods):
			msg := fmt.Sprintf("port %d → targetPort %s/%s found on %d/%d pod(s)", sp.Port, portString(tp), proto, matched, len(pods))
			if viaProbe > 0 {
				msg += fmt.Sprintf(" (via probe port on %d)", viaProbe)
			}
			hop.Add(Check{Name: name, Status: StatusOK, Message: msg})
		case tp.Type == intstr.Int && len(declared) == 0 && len(probes) == 0:
			hop.Add(Check{
				Name:    name,
				Status:  StatusInfo,
				Message: fmt.Sprintf("port %d → targetPort %d: selected pods declare no containerPorts or probes, cannot verify", sp.Port, tp.IntVal),
			})
		case tp.Type == intstr.Int && len(declared) == 0 && matched == 0:
			hop.Add(Check{
				Name:    name,
				Status:  StatusWarn,
				Message: fmt.Sprintf("port %d → targetPort %d/%s: pods declare no containerPorts and their probes use other ports", sp.Port, tp.IntVal, proto),
				Details: []string{"probes: " + joinKeys(probes)},
				Cause:   fmt.Sprintf("The health checks hit a different port than the one the Service forwards to (%d). One of the two is probably wrong.", tp.IntVal),
				Try:     []string{fmt.Sprintf("kubectl get pod %s -n %s -o jsonpath='{.spec.containers[*].readinessProbe}'", pods[0].Name, svc.Namespace)},
			})
		case matched > 0:
			hop.Add(Check{
				Name:    name,
				Status:  StatusWarn,
				Message: fmt.Sprintf("port %d → targetPort %s/%s found on only %d/%d pod(s)", sp.Port, portString(tp), proto, matched, len(pods)),
				Cause:   "Selected pods disagree on their ports, usually two versions of a workload behind one Service (rollout in progress, or a stray pod with matching labels).",
				Try:     []string{fmt.Sprintf("kubectl get pods -n %s -l %s -o custom-columns=NAME:.metadata.name,PORTS:.spec.containers[*].ports[*].containerPort", svc.Namespace, labels.SelectorFromSet(svc.Spec.Selector))},
			})
		default:
			c := Check{
				Name:    name,
				Status:  StatusFail,
				Message: fmt.Sprintf("port %d → targetPort %s/%s does not match any containerPort", sp.Port, portString(tp), proto),
				Details: portDetails(declared, probes),
				Try: []string{
					fmt.Sprintf("kubectl get svc %s -n %s -o jsonpath='{.spec.ports}'", svc.Name, svc.Namespace),
					fmt.Sprintf("kubectl get pod %s -n %s -o jsonpath='{.spec.containers[*].ports}'", pods[0].Name, svc.Namespace),
				},
			}
			if tp.Type == intstr.String {
				c.Cause = fmt.Sprintf("targetPort %q refers to a named port that no container defines, so the pods are left out of the Service's endpoints.", tp.StrVal)
			} else {
				c.Cause = fmt.Sprintf("Traffic is forwarded to port %d but the containers declare other ports; connections are likely refused. If the app really listens on %d without declaring it, add it to containerPorts.", tp.IntVal, tp.IntVal)
			}
			hop.Add(c)
		}
	}
}

// podHasPort reports whether any container in p exposes tp/proto, and records
// every declared port into declared for error messages.
func podHasPort(p corev1.Pod, tp intstr.IntOrString, proto corev1.Protocol, declared map[string]bool) bool {
	found := false
	for _, c := range p.Spec.Containers {
		for _, cp := range c.Ports {
			cpProto := cp.Protocol
			if cpProto == "" {
				cpProto = corev1.ProtocolTCP
			}
			label := fmt.Sprintf("%d/%s", cp.ContainerPort, cpProto)
			if cp.Name != "" {
				label = cp.Name + "=" + label
			}
			declared[label] = true
			if cpProto != proto {
				continue
			}
			if (tp.Type == intstr.String && cp.Name == tp.StrVal) ||
				(tp.Type == intstr.Int && cp.ContainerPort == tp.IntVal) {
				found = true
			}
		}
	}
	return found
}

// podProbesPort reports whether a probe on any container in p targets the
// numeric port tp, and records every numeric probe port into probes. Probes
// are always TCP; named probe ports resolve through containerPorts, which
// podHasPort already covers.
func podProbesPort(p corev1.Pod, tp intstr.IntOrString, proto corev1.Protocol, probes map[string]bool) bool {
	found := false
	for _, c := range p.Spec.Containers {
		for kind, pr := range map[string]*corev1.Probe{
			"readiness": c.ReadinessProbe, "liveness": c.LivenessProbe, "startup": c.StartupProbe,
		} {
			port, ok := probePort(pr)
			if !ok {
				continue
			}
			probes[fmt.Sprintf("%s %d/TCP", kind, port)] = true
			if tp.Type == intstr.Int && proto == corev1.ProtocolTCP && port == tp.IntVal {
				found = true
			}
		}
	}
	return found
}

func probePort(pr *corev1.Probe) (int32, bool) {
	if pr == nil {
		return 0, false
	}
	var port intstr.IntOrString
	switch {
	case pr.HTTPGet != nil:
		port = pr.HTTPGet.Port
	case pr.TCPSocket != nil:
		port = pr.TCPSocket.Port
	case pr.GRPC != nil:
		return pr.GRPC.Port, true
	default:
		return 0, false
	}
	if port.Type != intstr.Int || port.IntVal == 0 {
		return 0, false
	}
	return port.IntVal, true
}

func portDetails(declared, probes map[string]bool) []string {
	d := []string{"pods declare: " + joinKeys(declared)}
	if len(probes) > 0 {
		d = append(d, "probes: "+joinKeys(probes))
	}
	return d
}

func joinKeys(set map[string]bool) string {
	if len(set) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// checkEndpointSlices builds the EndpointSlices hop: how many backends are
// actually ready to receive traffic.
func (t *Tracer) checkEndpointSlices(ctx context.Context, svc *corev1.Service, selectedPods int) (Hop, error) {
	ns := svc.Namespace
	hop := Hop{Kind: "EndpointSlices", Namespace: ns, Name: svc.Name}

	list, err := t.Client.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + svc.Name,
	})
	if err != nil {
		return hop, fmt.Errorf("list endpointslices for %s/%s: %w", ns, svc.Name, err)
	}

	type backend struct {
		name  string
		ready bool
	}
	// The same pod can appear in an IPv4 and an IPv6 slice; count it once.
	backends := map[string]backend{}
	for _, s := range list.Items {
		for _, ep := range s.Endpoints {
			key, name := endpointIdentity(ep)
			if key == "" {
				continue
			}
			ready := ep.Conditions.Ready == nil || *ep.Conditions.Ready
			if prev, ok := backends[key]; ok {
				ready = ready || prev.ready
			}
			backends[key] = backend{name: name, ready: ready}
		}
	}

	var ready int
	var notReady []string
	for _, b := range backends {
		if b.ready {
			ready++
		} else {
			notReady = append(notReady, b.name)
		}
	}
	sort.Strings(notReady)
	total := len(backends)

	if svc.Spec.PublishNotReadyAddresses {
		hop.Add(Check{
			Name:    "publishNotReadyAddresses",
			Status:  StatusInfo,
			Message: "publishNotReadyAddresses is set: DNS also returns pods that are not ready",
		})
	}

	switch {
	case total == 0 && selectedPods > 0:
		hop.Add(Check{
			Name:    "endpoints",
			Status:  StatusFail,
			Message: fmt.Sprintf("0 endpoints although %d pod(s) match the selector", selectedPods),
			Cause:   "Pods are selected but none were published as endpoints. This usually means the targetPort cannot be resolved on them, or the endpoint controller has not caught up yet.",
			Try: []string{
				fmt.Sprintf("kubectl get endpointslices -n %s -l %s=%s -o yaml", ns, discoveryv1.LabelServiceName, svc.Name),
				fmt.Sprintf("kubectl describe svc %s -n %s", svc.Name, ns),
			},
		})
	case total == 0:
		hop.Add(Check{
			Name:    "endpoints",
			Status:  StatusFail,
			Message: "0 endpoints: nothing can receive traffic",
			Cause:   "The Service has no backends, so requests to it are rejected or time out.",
			Try:     []string{fmt.Sprintf("kubectl get endpointslices -n %s -l %s=%s", ns, discoveryv1.LabelServiceName, svc.Name)},
		})
	case ready == 0:
		hop.Add(Check{
			Name:    "endpoints",
			Status:  StatusFail,
			Message: fmt.Sprintf("0/%d endpoints ready", total),
			Details: []string{"not ready: " + strings.Join(notReady, ", ")},
			Cause:   "Every backend pod is failing its readiness check (or still starting), so the Service has nowhere to send traffic.",
			Try:     notReadyTry(ns, notReady),
		})
	case len(notReady) > 0:
		hop.Add(Check{
			Name:    "endpoints",
			Status:  StatusWarn,
			Message: fmt.Sprintf("%d/%d endpoints ready", ready, total),
			Details: []string{"not ready: " + strings.Join(notReady, ", ")},
			Cause:   "Some backends are not ready. Traffic still flows to the rest, with less capacity.",
			Try:     notReadyTry(ns, notReady),
		})
	default:
		hop.Add(Check{
			Name:    "endpoints",
			Status:  StatusOK,
			Message: fmt.Sprintf("%d/%d endpoints ready", ready, total),
		})
	}
	return hop, nil
}

// endpointIdentity returns a dedupe key and a display name for an endpoint,
// preferring the backing pod over its address.
func endpointIdentity(ep discoveryv1.Endpoint) (key, name string) {
	if ep.TargetRef != nil && ep.TargetRef.Name != "" {
		return ep.TargetRef.Kind + "/" + ep.TargetRef.Name, ep.TargetRef.Name
	}
	if len(ep.Addresses) > 0 {
		return ep.Addresses[0], ep.Addresses[0]
	}
	return "", ""
}

func notReadyTry(ns string, notReady []string) []string {
	if len(notReady) == 0 {
		return nil
	}
	return []string{
		fmt.Sprintf("kubectl describe pod %s -n %s", notReady[0], ns),
		fmt.Sprintf("kubectl get events -n %s --field-selector involvedObject.name=%s", ns, notReady[0]),
	}
}

func portString(p intstr.IntOrString) string { return p.String() }
