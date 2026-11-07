package trace

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	// maxPeerDetails caps how many allowed sources are listed for a port.
	maxPeerDetails = 5
	// enforcementNote qualifies every NetworkPolicy finding: the API only
	// stores policies, the CNI plugin is what enforces them.
	enforcementNote = "This only bites if the cluster's CNI enforces NetworkPolicy (Calico, Cilium, recent kindnet, ...)."
)

// trafficSource is who sends traffic to the backend pods, when that is known:
// the ingress controller's pods for a trace that starts at an Ingress.
type trafficSource struct {
	name string
	pods []corev1.Pod
	// port is the Service port the source sends to; nil means all ports.
	port *networkingv1.ServiceBackendPort
}

// sends reports whether the source sends traffic to Service port sp.
func (s *trafficSource) sends(sp corev1.ServicePort) bool {
	if s.port == nil {
		return true
	}
	if s.port.Name != "" {
		return s.port.Name == sp.Name
	}
	return s.port.Number == sp.Port
}

// verdict is whether NetworkPolicy admits one connection, ordered so that the
// most permissive outcome of several rules is the maximum.
type verdict int

const (
	denied verdict = iota
	// uncertain means only ipBlock peers could admit the connection, and
	// whether ipBlock applies to pod IPs depends on the CNI.
	uncertain
	allowed
)

// netpolHop checks the NetworkPolicies that isolate the pods behind svc for
// ingress. ok is false when no policy selects any of the pods: then all
// ingress is allowed and the hop would only add noise.
func (t *Tracer) netpolHop(ctx context.Context, svc *corev1.Service, pods []corev1.Pod, src *trafficSource) (hop Hop, ok bool, err error) {
	ns := svc.Namespace
	list, err := t.Client.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{})
	if apierrors.IsForbidden(err) {
		hop = Hop{Kind: "NetworkPolicy", Namespace: ns, Name: "*"}
		hop.Add(Check{Name: "policies", Status: StatusInfo, Message: "not allowed to list NetworkPolicies; not checked"})
		return hop, true, nil
	}
	if err != nil {
		return hop, false, fmt.Errorf("list networkpolicies in %s: %w", ns, err)
	}

	var policies []networkingv1.NetworkPolicy
	for _, np := range list.Items {
		if !governsIngress(&np) {
			continue
		}
		for i := range pods {
			if selectorMatches(&np.Spec.PodSelector, pods[i].Labels) {
				policies = append(policies, np)
				break
			}
		}
	}
	if len(policies) == 0 {
		return hop, false, nil
	}
	sort.Slice(policies, func(i, j int) bool { return policies[i].Name < policies[j].Name })

	names := make([]string, len(policies))
	for i, np := range policies {
		names[i] = np.Name
	}
	hop = Hop{Kind: "NetworkPolicy", Namespace: ns, Name: shortList(names, 2)}
	hop.Add(Check{
		Name:    "policies",
		Status:  StatusInfo,
		Message: fmt.Sprintf("%d %s the backing pods for ingress", len(policies), plural(len(policies), "policy isolates", "policies isolate")),
		Details: []string{strings.Join(names, ", ")},
	})

	var srcNS map[string]nsLabels
	if src != nil {
		if srcNS, err = t.namespaceLabels(ctx, src.pods); err != nil {
			return hop, false, err
		}
	}

	try := []string{
		fmt.Sprintf("kubectl describe networkpolicy -n %s %s", ns, strings.Join(names, " ")),
	}
	for _, sp := range svc.Spec.Ports {
		if src != nil {
			if !src.sends(sp) {
				continue // other ports have other clients, e.g. a metrics scraper
			}
			hop.Add(checkFromSource(sp, pods, policies, src, srcNS, try))
		} else {
			hop.Add(checkFromAnywhere(sp, pods, policies, try))
		}
	}
	return hop, true, nil
}

// governsIngress reports whether np restricts ingress. Without policyTypes,
// a policy always restricts ingress (and egress only if it has egress rules).
func governsIngress(np *networkingv1.NetworkPolicy) bool {
	if len(np.Spec.PolicyTypes) == 0 {
		return true
	}
	for _, pt := range np.Spec.PolicyTypes {
		if pt == networkingv1.PolicyTypeIngress {
			return true
		}
	}
	return false
}

// checkFromSource asks whether every source pod can reach every backend pod
// on the Service port.
func checkFromSource(sp corev1.ServicePort, pods []corev1.Pod, policies []networkingv1.NetworkPolicy, src *trafficSource, srcNS map[string]nsLabels, try []string) Check {
	c := Check{Name: portCheckName(sp)}
	var checked, blocked, unsure []string
	port := ""
	for i := range pods {
		p := &pods[i]
		num, name, ok := resolveTargetPort(sp, p)
		if !ok {
			continue // the Service hop already reports the unresolvable port
		}
		port = describePort(num, name, sp.Protocol)
		applicable := policiesFor(policies, p)
		worst := allowed
		for j := range src.pods {
			s := &src.pods[j]
			v := allowed
			if len(applicable) > 0 {
				v = admitsPod(applicable, s, srcNS[s.Namespace], num, name, protocol(sp.Protocol))
			}
			worst = min(worst, v)
		}
		checked = append(checked, p.Name)
		switch worst {
		case denied:
			blocked = append(blocked, p.Name)
		case uncertain:
			unsure = append(unsure, p.Name)
		}
	}

	namespaces := map[string]bool{}
	for _, s := range src.pods {
		namespaces[s.Namespace] = true
	}
	who := fmt.Sprintf("%s pods (namespace %s)", src.name, joinKeys(namespaces))

	switch {
	case len(checked) == 0:
		c.Status = StatusSkip
		c.Message = "target port does not resolve on any pod; not evaluated"
	case len(blocked) == len(checked):
		c.Status = StatusFail
		c.Message = fmt.Sprintf("no rule admits %s on %s", who, port)
		c.Cause = fmt.Sprintf("A NetworkPolicy isolates the backend pods and none of its ingress rules admits %s on %s, so the controller's connections are dropped and it answers 502 or 504. Add a rule whose from matches the controller's namespace and pod labels. %s", who, port, enforcementNote)
		c.Try = slices.Concat(try, []string{
			fmt.Sprintf("kubectl get pods -n %s --show-labels", joinKeys(namespaces)),
			fmt.Sprintf("kubectl get ns %s --show-labels", joinKeys(namespaces)),
		})
	case len(blocked) > 0:
		c.Status = StatusWarn
		c.Message = fmt.Sprintf("%d/%d pods do not admit %s on %s", len(blocked), len(checked), who, port)
		c.Details = []string{"blocked: " + shortList(blocked, maxPodChecks)}
		c.Cause = fmt.Sprintf("The NetworkPolicies treat the backend pods differently, so requests load-balanced to %s are dropped. %s", shortList(blocked, 2), enforcementNote)
		c.Try = try
	case len(unsure) > 0:
		c.Status = StatusInfo
		c.Message = fmt.Sprintf("cannot decide whether %s may connect on %s", who, port)
		c.Details = []string{"only ipBlock rules (whose effect on pod IPs depends on the CNI) or namespace labels that could not be read would admit it"}
	default:
		c.Status = StatusOK
		c.Message = fmt.Sprintf("admits %s on %s", who, port)
	}
	return c
}

// checkFromAnywhere is used when the client is unknown: it reports whether
// the port is open to all, open to some sources, or closed to everyone.
func checkFromAnywhere(sp corev1.ServicePort, pods []corev1.Pod, policies []networkingv1.NetworkPolicy, try []string) Check {
	c := Check{Name: portCheckName(sp)}
	var checked, closed []string
	open := true
	peers := map[string]bool{}
	port := ""
	for i := range pods {
		p := &pods[i]
		num, name, ok := resolveTargetPort(sp, p)
		if !ok {
			continue
		}
		port = describePort(num, name, sp.Protocol)
		checked = append(checked, p.Name)
		applicable := policiesFor(policies, p)
		if len(applicable) == 0 {
			continue
		}
		podOpen, podPeers := openness(applicable, num, name, protocol(sp.Protocol))
		if !podOpen && len(podPeers) == 0 {
			closed = append(closed, p.Name)
		}
		if !podOpen {
			open = false
		}
		for _, peer := range podPeers {
			peers[peer] = true
		}
	}

	switch {
	case len(checked) == 0:
		c.Status = StatusSkip
		c.Message = "target port does not resolve on any pod; not evaluated"
	case len(closed) == len(checked):
		c.Status = StatusFail
		c.Message = fmt.Sprintf("no ingress rule admits any traffic on %s", port)
		c.Cause = fmt.Sprintf("A NetworkPolicy isolates the backend pods and none of its ingress rules covers %s, so every connection is dropped. A default-deny policy needs a companion policy that allows the real clients. %s", port, enforcementNote)
		c.Try = slices.Concat(try, []string{fmt.Sprintf("kubectl get networkpolicy -n %s", pods[0].Namespace)})
	case len(closed) > 0:
		c.Status = StatusWarn
		c.Message = fmt.Sprintf("%d/%d pods admit no traffic on %s", len(closed), len(checked), port)
		c.Details = []string{"closed: " + shortList(closed, maxPodChecks)}
		c.Cause = fmt.Sprintf("The NetworkPolicies treat the backend pods differently, so requests load-balanced to %s are dropped. %s", shortList(closed, 2), enforcementNote)
		c.Try = try
	case open:
		c.Status = StatusOK
		c.Message = fmt.Sprintf("open to all sources on %s", port)
	default:
		sorted := make([]string, 0, len(peers))
		for p := range peers {
			sorted = append(sorted, p)
		}
		sort.Strings(sorted)
		c.Status = StatusInfo
		c.Message = fmt.Sprintf("%s admitted only from selected sources; make sure the real clients are among them", port)
		if len(sorted) > maxPeerDetails {
			sorted = append(sorted[:maxPeerDetails], fmt.Sprintf("and %d more", len(sorted)-maxPeerDetails))
		}
		c.Details = sorted
	}
	return c
}

// admitsPod evaluates the policies that select a backend pod against one
// source pod: the connection is admitted if any rule of any policy admits it.
func admitsPod(policies []networkingv1.NetworkPolicy, src *corev1.Pod, srcNS nsLabels, port int32, portName string, proto corev1.Protocol) verdict {
	best := denied
	for i := range policies {
		np := &policies[i]
		for _, rule := range np.Spec.Ingress {
			if !portMatches(rule.Ports, port, portName, proto) {
				continue
			}
			if len(rule.From) == 0 {
				return allowed
			}
			for _, peer := range rule.From {
				best = max(best, peerAdmits(peer, np.Namespace, src, srcNS))
				if best == allowed {
					return allowed
				}
			}
		}
	}
	return best
}

// peerAdmits matches one from entry against a source pod.
func peerAdmits(peer networkingv1.NetworkPolicyPeer, policyNS string, src *corev1.Pod, srcNS nsLabels) verdict {
	if peer.IPBlock != nil {
		return uncertain
	}
	if peer.PodSelector != nil && !selectorMatches(peer.PodSelector, src.Labels) {
		return denied
	}
	switch {
	case peer.NamespaceSelector == nil:
		// A bare podSelector only selects pods in the policy's own namespace.
		if src.Namespace != policyNS {
			return denied
		}
	case selectorMatches(peer.NamespaceSelector, srcNS.set):
	case srcNS.partial:
		// The selector may use labels we could not read.
		return uncertain
	default:
		return denied
	}
	return allowed
}

// openness reports whether some rule admits every source on the port and,
// if not, describes the sources that are admitted.
func openness(policies []networkingv1.NetworkPolicy, port int32, portName string, proto corev1.Protocol) (open bool, peers []string) {
	for i := range policies {
		np := &policies[i]
		for _, rule := range np.Spec.Ingress {
			if !portMatches(rule.Ports, port, portName, proto) {
				continue
			}
			if len(rule.From) == 0 {
				return true, nil
			}
			for _, peer := range rule.From {
				peers = append(peers, describePeer(peer, np.Namespace))
			}
		}
	}
	return false, peers
}

// portMatches reports whether a rule's ports cover the backend port. No ports
// means all ports; a named port refers to the backend container's port name.
func portMatches(ports []networkingv1.NetworkPolicyPort, port int32, portName string, proto corev1.Protocol) bool {
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		pp := corev1.ProtocolTCP
		if p.Protocol != nil {
			pp = *p.Protocol
		}
		if pp != proto {
			continue
		}
		switch {
		case p.Port == nil:
			return true
		case p.Port.StrVal != "":
			if p.Port.StrVal == portName {
				return true
			}
		case p.EndPort != nil:
			if port >= p.Port.IntVal && port <= *p.EndPort {
				return true
			}
		case p.Port.IntVal == port:
			return true
		}
	}
	return false
}

func policiesFor(policies []networkingv1.NetworkPolicy, p *corev1.Pod) []networkingv1.NetworkPolicy {
	var out []networkingv1.NetworkPolicy
	for _, np := range policies {
		if selectorMatches(&np.Spec.PodSelector, p.Labels) {
			out = append(out, np)
		}
	}
	return out
}

// resolveTargetPort finds the container port number and name that a Service
// port sends traffic to on pod p.
func resolveTargetPort(sp corev1.ServicePort, p *corev1.Pod) (num int32, name string, ok bool) {
	tp := effectiveTargetPort(sp)
	proto := protocol(sp.Protocol)
	for _, ctr := range p.Spec.Containers {
		for _, cp := range ctr.Ports {
			if protocol(cp.Protocol) != proto {
				continue
			}
			if tp.StrVal != "" && cp.Name == tp.StrVal {
				return cp.ContainerPort, cp.Name, true
			}
			if tp.StrVal == "" && cp.ContainerPort == tp.IntVal {
				return cp.ContainerPort, cp.Name, true
			}
		}
	}
	if tp.StrVal == "" {
		// A numeric targetPort works without a matching containerPort.
		return tp.IntVal, "", true
	}
	return 0, "", false
}

// nsLabels are the labels of a source namespace. partial means the
// Namespace could not be read and only the well-known name label is known.
type nsLabels struct {
	set     labels.Set
	partial bool
}

// namespaceLabels reads the labels of the namespaces the given pods run in.
// Without permission it falls back to the label every namespace carries.
func (t *Tracer) namespaceLabels(ctx context.Context, pods []corev1.Pod) (map[string]nsLabels, error) {
	out := map[string]nsLabels{}
	for _, p := range pods {
		if _, seen := out[p.Namespace]; seen {
			continue
		}
		l := nsLabels{set: labels.Set{}}
		nsObj, err := t.Client.CoreV1().Namespaces().Get(ctx, p.Namespace, metav1.GetOptions{})
		switch {
		case err == nil:
			for k, v := range nsObj.Labels {
				l.set[k] = v
			}
		case apierrors.IsForbidden(err), apierrors.IsNotFound(err):
			l.partial = true
		default:
			return nil, fmt.Errorf("get namespace %s: %w", p.Namespace, err)
		}
		l.set[corev1.LabelMetadataName] = p.Namespace
		out[p.Namespace] = l
	}
	return out, nil
}

func selectorMatches(sel *metav1.LabelSelector, lbls map[string]string) bool {
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return false
	}
	return s.Matches(labels.Set(lbls))
}

func describePeer(peer networkingv1.NetworkPolicyPeer, policyNS string) string {
	if peer.IPBlock != nil {
		desc := "ipBlock " + peer.IPBlock.CIDR
		if len(peer.IPBlock.Except) > 0 {
			desc += " except " + strings.Join(peer.IPBlock.Except, ", ")
		}
		return desc
	}
	pods := "all pods"
	if peer.PodSelector != nil && (len(peer.PodSelector.MatchLabels) > 0 || len(peer.PodSelector.MatchExpressions) > 0) {
		pods = "pods " + metav1.FormatLabelSelector(peer.PodSelector)
	}
	switch {
	case peer.NamespaceSelector == nil:
		return pods + " in namespace " + policyNS
	case len(peer.NamespaceSelector.MatchLabels) == 0 && len(peer.NamespaceSelector.MatchExpressions) == 0:
		return pods + " in all namespaces"
	default:
		return pods + " in namespaces " + metav1.FormatLabelSelector(peer.NamespaceSelector)
	}
}

func portCheckName(sp corev1.ServicePort) string {
	return fmt.Sprintf("port %d", sp.Port)
}

func describePort(num int32, name string, proto corev1.Protocol) string {
	if name != "" {
		return fmt.Sprintf("%d/%s (%s)", num, protocol(proto), name)
	}
	return fmt.Sprintf("%d/%s", num, protocol(proto))
}

// protocol applies the API default of TCP.
func protocol(p corev1.Protocol) corev1.Protocol {
	if p == "" {
		return corev1.ProtocolTCP
	}
	return p
}

// shortList joins up to n names and summarizes the rest.
func shortList(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s +%d more", strings.Join(names[:n], ", "), len(names)-n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
