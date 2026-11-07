package trace

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// --- fixtures ---------------------------------------------------------------

type npOpt func(*networkingv1.NetworkPolicy)

// netpol selects the web pods for ingress; with no options it denies all.
func netpol(name string, opts ...npOpt) *networkingv1.NetworkPolicy {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: webLabels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
	for _, o := range opts {
		o(np)
	}
	return np
}

func allowFrom(ports []networkingv1.NetworkPolicyPort, peers ...networkingv1.NetworkPolicyPeer) npOpt {
	return func(np *networkingv1.NetworkPolicy) {
		np.Spec.Ingress = append(np.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{Ports: ports, From: peers})
	}
}

func selectingPods(sel map[string]string) npOpt {
	return func(np *networkingv1.NetworkPolicy) { np.Spec.PodSelector = metav1.LabelSelector{MatchLabels: sel} }
}

func policyTypes(types ...networkingv1.PolicyType) npOpt {
	return func(np *networkingv1.NetworkPolicy) { np.Spec.PolicyTypes = types }
}

func tcpPort(p intstr.IntOrString) networkingv1.NetworkPolicyPort {
	return networkingv1.NetworkPolicyPort{Port: &p}
}

func portRange(from, to int32) networkingv1.NetworkPolicyPort {
	p := intstr.FromInt32(from)
	return networkingv1.NetworkPolicyPort{Port: &p, EndPort: &to}
}

func podPeer(sel map[string]string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: sel}}
}

func nsPeer(sel map[string]string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: sel}}
}

func namespace(name string, lbls map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbls}}
}

var controllerNS = map[string]string{corev1.LabelMetadataName: "ingress-nginx"}

// --- tests ------------------------------------------------------------------

func TestNetworkPolicyFromService(t *testing.T) {
	tests := []struct {
		name       string
		objects    []runtime.Object
		wantHop    bool
		wantStatus Status
		wantCheck  check
		wantCause  string
	}{
		{
			name:       "no policies, no hop",
			objects:    healthyBackend(),
			wantStatus: StatusOK,
		},
		{
			name:       "policy for other pods, no hop",
			objects:    append(healthyBackend(), netpol("db-only", selectingPods(map[string]string{"app": "db"}))),
			wantStatus: StatusOK,
		},
		{
			name:       "egress-only policy, no hop",
			objects:    append(healthyBackend(), netpol("egress", policyTypes(networkingv1.PolicyTypeEgress))),
			wantStatus: StatusOK,
		},
		{
			name:       "default deny closes the port",
			objects:    append(healthyBackend(), netpol("default-deny", selectingPods(nil))),
			wantHop:    true,
			wantStatus: StatusFail,
			wantCheck:  check{"NetworkPolicy", "port 80", StatusFail, "no ingress rule admits any traffic on 8080/TCP (http)"},
			wantCause:  "default-deny policy needs a companion policy",
		},
		{
			name:       "rule for another port",
			objects:    append(healthyBackend(), netpol("metrics", allowFrom([]networkingv1.NetworkPolicyPort{tcpPort(intstr.FromInt32(9090))}))),
			wantHop:    true,
			wantStatus: StatusFail,
			wantCheck:  check{"NetworkPolicy", "port 80", StatusFail, "no ingress rule admits"},
		},
		{
			name:       "open to all on the named port",
			objects:    append(healthyBackend(), netpol("allow-http", allowFrom([]networkingv1.NetworkPolicyPort{tcpPort(intstr.FromString("http"))}))),
			wantHop:    true,
			wantStatus: StatusOK,
			wantCheck:  check{"NetworkPolicy", "port 80", StatusOK, "open to all sources on 8080/TCP (http)"},
		},
		{
			name:       "port range covers the target port",
			objects:    append(healthyBackend(), netpol("range", allowFrom([]networkingv1.NetworkPolicyPort{portRange(8000, 8100)}))),
			wantHop:    true,
			wantStatus: StatusOK,
			wantCheck:  check{"NetworkPolicy", "port 80", StatusOK, "open to all"},
		},
		{
			name:       "restricted to selected sources",
			objects:    append(healthyBackend(), netpol("from-frontend", allowFrom(nil, podPeer(map[string]string{"app": "frontend"})))),
			wantHop:    true,
			wantStatus: StatusOK,
			wantCheck:  check{"NetworkPolicy", "port 80", StatusInfo, "admitted only from selected sources pods app=frontend in namespace shop"},
		},
		{
			name: "deny plus allow is still open",
			objects: append(healthyBackend(),
				netpol("default-deny", selectingPods(nil)),
				netpol("allow-all", allowFrom(nil)),
			),
			wantHop:    true,
			wantStatus: StatusOK,
			wantCheck:  check{"NetworkPolicy", "policies", StatusInfo, "2 policies isolate allow-all, default-deny"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &Tracer{Client: fake.NewClientset(tc.objects...)}
			r, err := tr.Trace(context.Background(), Target{Kind: KindService, Namespace: ns, Name: "web"})
			if err != nil {
				t.Fatalf("Trace: %v", err)
			}
			hasHop := false
			for _, h := range r.Hops {
				hasHop = hasHop || h.Kind == "NetworkPolicy"
			}
			if hasHop != tc.wantHop {
				t.Fatalf("NetworkPolicy hop present = %v, want %v\n%s", hasHop, tc.wantHop, dump(r))
			}
			if r.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s\n%s", r.Status, tc.wantStatus, dump(r))
			}
			if tc.wantCheck.hop != "" {
				assertCheck(t, r, tc.wantCheck)
			}
			if tc.wantCause != "" {
				c := r.PrimaryIssue()
				if c == nil || !strings.Contains(c.Cause, tc.wantCause) || len(c.Try) == 0 {
					t.Errorf("primary issue = %+v, want cause containing %q and Try commands", c, tc.wantCause)
				}
			}
		})
	}
}

func TestNetworkPolicyFromIngressController(t *testing.T) {
	ports := []networkingv1.NetworkPolicyPort{tcpPort(intstr.FromInt32(8080))}
	tests := []struct {
		name      string
		objects   []runtime.Object
		wantCheck check
		wantCause string
	}{
		{
			name:      "default deny blocks the controller",
			objects:   healthyCluster(ingress(), netpol("default-deny")),
			wantCheck: check{"NetworkPolicy", "port 80", StatusFail, "no rule admits ingress-nginx pods (namespace ingress-nginx) on 8080/TCP (http)"},
			wantCause: "the controller's connections are dropped",
		},
		{
			name:      "namespace selector on the well-known name label",
			objects:   healthyCluster(ingress(), netpol("from-ingress", allowFrom(ports, nsPeer(controllerNS)))),
			wantCheck: check{"NetworkPolicy", "port 80", StatusOK, "admits ingress-nginx pods"},
		},
		{
			name: "namespace selector on a custom label",
			objects: healthyCluster(ingress(),
				namespace("ingress-nginx", map[string]string{"role": "edge"}),
				netpol("from-edge", allowFrom(ports, nsPeer(map[string]string{"role": "edge"}))),
			),
			wantCheck: check{"NetworkPolicy", "port 80", StatusOK, "admits"},
		},
		{
			name: "namespace and pod selector must both match",
			objects: healthyCluster(ingress(), netpol("from-ingress", allowFrom(ports, networkingv1.NetworkPolicyPeer{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: controllerNS},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "traefik"}},
			}))),
			wantCheck: check{"NetworkPolicy", "port 80", StatusFail, "no rule admits"},
		},
		{
			name:      "bare pod selector only covers the policy's namespace",
			objects:   healthyCluster(ingress(), netpol("same-ns", allowFrom(ports, podPeer(map[string]string{"app.kubernetes.io/name": "ingress-nginx"})))),
			wantCheck: check{"NetworkPolicy", "port 80", StatusFail, "no rule admits"},
			wantCause: "Add a rule whose from matches the controller's namespace",
		},
		{
			name:      "wrong port",
			objects:   healthyCluster(ingress(), netpol("from-ingress", allowFrom([]networkingv1.NetworkPolicyPort{tcpPort(intstr.FromInt32(9090))}, nsPeer(controllerNS)))),
			wantCheck: check{"NetworkPolicy", "port 80", StatusFail, "no rule admits"},
		},
		{
			name: "ipBlock only is undecidable",
			objects: healthyCluster(ingress(), netpol("cidr", allowFrom(ports, networkingv1.NetworkPolicyPeer{
				IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8"},
			}))),
			wantCheck: check{"NetworkPolicy", "port 80", StatusInfo, "cannot decide ipBlock"},
		},
		{
			name:      "unreadable namespace labels are undecidable",
			objects:   healthyCluster(ingress(), netpol("from-edge", allowFrom(ports, nsPeer(map[string]string{"role": "edge"})))),
			wantCheck: check{"NetworkPolicy", "port 80", StatusInfo, "cannot decide namespace labels"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &Tracer{Client: fake.NewClientset(tc.objects...), Options: Options{Host: "shop.example.com"}}
			r, err := tr.Trace(context.Background(), Target{Kind: KindIngress, Namespace: ns, Name: "shop"})
			if err != nil {
				t.Fatalf("Trace: %v", err)
			}
			assertCheck(t, r, tc.wantCheck)
			// A full block makes the NetworkPolicy hop (Ingress, Service,
			// EndpointSlices, NetworkPolicy, Pods) the first broken one.
			wantBroken := -1
			if tc.wantCheck.status == StatusFail {
				wantBroken = 3
			}
			if r.FirstBroken != wantBroken {
				t.Errorf("firstBroken = %d, want %d\n%s", r.FirstBroken, wantBroken, dump(r))
			}
			if tc.wantCause != "" {
				c := r.PrimaryIssue()
				if c == nil || !strings.Contains(c.Cause, tc.wantCause) || len(c.Try) == 0 {
					t.Errorf("primary issue = %+v, want cause containing %q and Try commands", c, tc.wantCause)
				}
			}
		})
	}
}

// Only the port the Ingress uses is checked against the controller; other
// Service ports have other clients.
func TestNetworkPolicyIngressBackendPortOnly(t *testing.T) {
	withMetrics := func(s *corev1.Service) {
		s.Spec.Ports = append(s.Spec.Ports, corev1.ServicePort{Name: "metrics", Port: 9090, TargetPort: intstr.FromString("metrics")})
	}
	objects := []runtime.Object{
		ingressClass("nginx", "k8s.io/ingress-nginx", false),
		controllerPod("ingress-nginx-controller-1", true),
		service("web", withMetrics),
		pod("web-1", webLabels, withPorts(
			corev1.ContainerPort{Name: "http", ContainerPort: 8080},
			corev1.ContainerPort{Name: "metrics", ContainerPort: 9090},
		)),
		slice("web-abc", "web", ep{"web-1", "10.0.0.1", true}),
		ingress(),
		netpol("from-ingress", allowFrom([]networkingv1.NetworkPolicyPort{tcpPort(intstr.FromString("http"))}, nsPeer(controllerNS))),
		netpol("from-prometheus", allowFrom([]networkingv1.NetworkPolicyPort{tcpPort(intstr.FromString("metrics"))}, nsPeer(map[string]string{corev1.LabelMetadataName: "monitoring"}))),
	}
	tr := &Tracer{Client: fake.NewClientset(objects...), Options: Options{Host: "shop.example.com"}}
	r, err := tr.Trace(context.Background(), Target{Kind: KindIngress, Namespace: ns, Name: "shop"})
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	assertCheck(t, r, check{"NetworkPolicy", "port 80", StatusOK, "admits ingress-nginx pods"})
	if c := findCheck(r, "NetworkPolicy", "port 9090"); c != nil {
		t.Errorf("metrics port checked against the ingress controller: %+v", c)
	}
	if r.Status != StatusOK {
		t.Errorf("status = %s, want ok\n%s", r.Status, dump(r))
	}
}

func TestNetworkPolicyHopOrder(t *testing.T) {
	tr := &Tracer{Client: fake.NewClientset(append(healthyBackend(), netpol("default-deny"))...)}
	r, err := tr.Trace(context.Background(), Target{Kind: KindService, Namespace: ns, Name: "web"})
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	var kinds []string
	for _, h := range r.Hops {
		kinds = append(kinds, h.Kind)
	}
	if got, want := strings.Join(kinds, ","), "Service,EndpointSlices,NetworkPolicy,Pods"; got != want {
		t.Errorf("hops = %s, want %s", got, want)
	}
}

func TestNetworkPolicyForbidden(t *testing.T) {
	client := fake.NewClientset(append(healthyBackend(), netpol("default-deny"))...)
	client.PrependReactor("list", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "", nil)
	})
	tr := &Tracer{Client: client}
	r, err := tr.Trace(context.Background(), Target{Kind: KindService, Namespace: ns, Name: "web"})
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	assertCheck(t, r, check{"NetworkPolicy", "policies", StatusInfo, "not allowed to list NetworkPolicies"})
	if r.Status != StatusOK {
		t.Errorf("status = %s, want ok\n%s", r.Status, dump(r))
	}
}

func TestPortMatches(t *testing.T) {
	udp := corev1.ProtocolUDP
	tests := []struct {
		name  string
		ports []networkingv1.NetworkPolicyPort
		want  bool
	}{
		{"no ports means all", nil, true},
		{"number", []networkingv1.NetworkPolicyPort{tcpPort(intstr.FromInt32(8080))}, true},
		{"other number", []networkingv1.NetworkPolicyPort{tcpPort(intstr.FromInt32(80))}, false},
		{"name", []networkingv1.NetworkPolicyPort{tcpPort(intstr.FromString("http"))}, true},
		{"other name", []networkingv1.NetworkPolicyPort{tcpPort(intstr.FromString("metrics"))}, false},
		{"range", []networkingv1.NetworkPolicyPort{portRange(8000, 9000)}, true},
		{"range below", []networkingv1.NetworkPolicyPort{portRange(9000, 9100)}, false},
		{"protocol only", []networkingv1.NetworkPolicyPort{{}}, true},
		{"other protocol", []networkingv1.NetworkPolicyPort{{Protocol: &udp}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := portMatches(tc.ports, 8080, "http", corev1.ProtocolTCP); got != tc.want {
				t.Errorf("portMatches = %v, want %v", got, tc.want)
			}
		})
	}
}
