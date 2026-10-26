package trace

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

const ns = "shop"

// --- fixtures ---------------------------------------------------------------

type svcOpt func(*corev1.Service)

func service(name string, opts ...svcOpt) *corev1.Service {
	s := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.96.0.10",
			Selector:  map[string]string{"app": name},
			Ports:     []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromString("http")}},
		},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func withSelector(sel map[string]string) svcOpt {
	return func(s *corev1.Service) { s.Spec.Selector = sel }
}

func withTargetPort(tp intstr.IntOrString) svcOpt {
	return func(s *corev1.Service) { s.Spec.Ports[0].TargetPort = tp }
}

type podOpt func(*corev1.Pod)

func pod(name string, lbls map[string]string, opts ...podOpt) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: lbls},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "app",
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func withPorts(ports ...corev1.ContainerPort) podOpt {
	return func(p *corev1.Pod) { p.Spec.Containers[0].Ports = ports }
}

func withPhase(ph corev1.PodPhase) podOpt {
	return func(p *corev1.Pod) { p.Status.Phase = ph }
}

type ep struct {
	pod   string
	ip    string
	ready bool
}

func slice(name, svc string, eps ...ep) *discoveryv1.EndpointSlice {
	s := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{discoveryv1.LabelServiceName: svc},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	for _, e := range eps {
		ready := e.ready
		end := discoveryv1.Endpoint{
			Addresses:  []string{e.ip},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		}
		if e.pod != "" {
			end.TargetRef = &corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: e.pod}
		}
		s.Endpoints = append(s.Endpoints, end)
	}
	return s
}

var webLabels = map[string]string{"app": "web"}

// --- tests ------------------------------------------------------------------

func TestTraceService(t *testing.T) {
	type wantCheck struct {
		hop, name string
		status    Status
		contains  string
	}
	tests := []struct {
		name        string
		objects     []runtime.Object
		wantStatus  Status
		wantBroken  int
		wantHops    int
		wantChecks  []wantCheck
		wantExit    int
		wantCauseIn string
	}{
		{
			name: "healthy named targetPort",
			objects: []runtime.Object{
				service("web"),
				pod("web-1", webLabels),
				pod("web-2", webLabels),
				slice("web-abc", "web", ep{"web-1", "10.0.0.1", true}, ep{"web-2", "10.0.0.2", true}),
			},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 2, wantExit: ExitOK,
			wantChecks: []wantCheck{
				{"Service", "exists", StatusOK, "ClusterIP, ports 80→http"},
				{"Service", "selector", StatusOK, "matches 2 pod(s)"},
				{"Service", "targetPort http", StatusOK, "found on 2/2"},
				{"EndpointSlices", "endpoints", StatusOK, "2/2 endpoints ready"},
			},
		},
		{
			name:       "service missing",
			objects:    nil,
			wantStatus: StatusFail, wantBroken: 0, wantHops: 1, wantExit: ExitBroken,
			wantChecks: []wantCheck{
				{"Service", "exists", StatusFail, `Service "web" not found`},
			},
			wantCauseIn: "does not exist",
		},
		{
			name: "selector matches no pods",
			objects: []runtime.Object{
				service("web", withSelector(map[string]string{"app": "web", "tier": "frontend"})),
				pod("web-1", map[string]string{"app": "web-v2", "tier": "frontend"}),
			},
			wantStatus: StatusFail, wantBroken: 0, wantHops: 2, wantExit: ExitBroken,
			wantChecks: []wantCheck{
				{"Service", "selector", StatusFail, "app=web found instead: app=web-v2"},
				{"Service", "selector", StatusFail, "tier=frontend matches 1 pod(s)"},
				{"EndpointSlices", "endpoints", StatusFail, "nothing can receive traffic"},
			},
			wantCauseIn: "selector does not match",
		},
		{
			name: "empty namespace",
			objects: []runtime.Object{
				service("web"),
			},
			wantStatus: StatusFail, wantBroken: 0, wantHops: 2, wantExit: ExitBroken,
			wantChecks: []wantCheck{
				{"Service", "selector", StatusFail, "has no pods at all"},
			},
		},
		{
			name: "completed pods are not backends",
			objects: []runtime.Object{
				service("web"),
				pod("web-job", webLabels, withPhase(corev1.PodSucceeded)),
			},
			wantStatus: StatusFail, wantBroken: 0, wantHops: 2, wantExit: ExitBroken,
			wantChecks: []wantCheck{
				{"Service", "selector", StatusFail, "matches 0 pods"},
			},
		},
		{
			name: "wrong numeric targetPort",
			objects: []runtime.Object{
				service("web", withTargetPort(intstr.FromInt32(8081))),
				pod("web-1", webLabels),
				slice("web-abc", "web", ep{"web-1", "10.0.0.1", true}),
			},
			wantStatus: StatusFail, wantBroken: 0, wantHops: 2, wantExit: ExitBroken,
			wantChecks: []wantCheck{
				{"Service", "targetPort http", StatusFail, "8081/TCP does not match any containerPort pods declare: http=8080/TCP"},
				{"EndpointSlices", "endpoints", StatusOK, "1/1"},
			},
			wantCauseIn: "Traffic is forwarded to port 8081",
		},
		{
			name: "unset targetPort defaults to port",
			objects: []runtime.Object{
				service("web", withTargetPort(intstr.IntOrString{})),
				pod("web-1", webLabels, withPorts(corev1.ContainerPort{ContainerPort: 80})),
				slice("web-abc", "web", ep{"web-1", "10.0.0.1", true}),
			},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 2, wantExit: ExitOK,
			wantChecks: []wantCheck{
				{"Service", "targetPort http", StatusOK, "targetPort 80/TCP"},
			},
		},
		{
			name: "protocol must match",
			objects: []runtime.Object{
				service("dns", withSelector(webLabels), func(s *corev1.Service) {
					s.Spec.Ports[0] = corev1.ServicePort{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP}
				}),
				pod("web-1", webLabels, withPorts(corev1.ContainerPort{ContainerPort: 53, Protocol: corev1.ProtocolTCP})),
				slice("dns-abc", "dns", ep{"web-1", "10.0.0.1", true}),
			},
			wantStatus: StatusFail, wantBroken: 0, wantHops: 2, wantExit: ExitBroken,
			wantChecks: []wantCheck{
				{"Service", "targetPort dns", StatusFail, "53/UDP does not match"},
			},
		},
		{
			name: "named targetPort missing on pods",
			objects: []runtime.Object{
				service("web"),
				pod("web-1", webLabels, withPorts(corev1.ContainerPort{Name: "web", ContainerPort: 8080})),
			},
			wantStatus: StatusFail, wantBroken: 0, wantHops: 2, wantExit: ExitBroken,
			wantChecks: []wantCheck{
				{"Service", "targetPort http", StatusFail, "http/TCP does not match"},
				{"EndpointSlices", "endpoints", StatusFail, "0 endpoints although 1 pod(s)"},
			},
			wantCauseIn: `targetPort "http" refers to a named port`,
		},
		{
			name: "targetPort on only some pods",
			objects: []runtime.Object{
				service("web"),
				pod("web-1", webLabels),
				pod("web-old", webLabels, withPorts(corev1.ContainerPort{Name: "web", ContainerPort: 8080})),
				slice("web-abc", "web", ep{"web-1", "10.0.0.1", true}),
			},
			wantStatus: StatusWarn, wantBroken: -1, wantHops: 2, wantExit: ExitWarning,
			wantChecks: []wantCheck{
				{"Service", "targetPort http", StatusWarn, "found on only 1/2"},
			},
		},
		{
			name: "pods declare no containerPorts",
			objects: []runtime.Object{
				service("web", withTargetPort(intstr.FromInt32(8080))),
				pod("web-1", webLabels, withPorts()),
				slice("web-abc", "web", ep{"web-1", "10.0.0.1", true}),
			},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 2, wantExit: ExitOK,
			wantChecks: []wantCheck{
				{"Service", "targetPort http", StatusInfo, "cannot verify"},
			},
		},
		{
			name: "no endpoint ready",
			objects: []runtime.Object{
				service("web"),
				pod("web-1", webLabels),
				pod("web-2", webLabels),
				slice("web-abc", "web", ep{"web-1", "10.0.0.1", false}, ep{"web-2", "10.0.0.2", false}),
			},
			wantStatus: StatusFail, wantBroken: 1, wantHops: 2, wantExit: ExitBroken,
			wantChecks: []wantCheck{
				{"Service", "selector", StatusOK, ""},
				{"EndpointSlices", "endpoints", StatusFail, "0/2 endpoints ready not ready: web-1, web-2"},
			},
			wantCauseIn: "failing its readiness check",
		},
		{
			name: "some endpoints not ready",
			objects: []runtime.Object{
				service("web"),
				pod("web-1", webLabels),
				pod("web-2", webLabels),
				slice("web-abc", "web", ep{"web-1", "10.0.0.1", true}, ep{"web-2", "10.0.0.2", false}),
			},
			wantStatus: StatusWarn, wantBroken: -1, wantHops: 2, wantExit: ExitWarning,
			wantChecks: []wantCheck{
				{"EndpointSlices", "endpoints", StatusWarn, "1/2 endpoints ready"},
			},
		},
		{
			name: "dual-stack slices count a pod once",
			objects: []runtime.Object{
				service("web"),
				pod("web-1", webLabels),
				slice("web-v4", "web", ep{"web-1", "10.0.0.1", true}),
				slice("web-v6", "web", ep{"web-1", "fd00::1", true}),
				slice("other", "api", ep{"api-1", "10.0.0.9", false}),
			},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 2, wantExit: ExitOK,
			wantChecks: []wantCheck{
				{"EndpointSlices", "endpoints", StatusOK, "1/1 endpoints ready"},
			},
		},
		{
			name: "ExternalName stops the trace",
			objects: []runtime.Object{
				service("db", func(s *corev1.Service) {
					s.Spec = corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "db.example.com"}
				}),
			},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 1, wantExit: ExitOK,
			wantChecks: []wantCheck{
				{"Service", "externalName", StatusInfo, "db.example.com"},
			},
		},
		{
			name: "headless service",
			objects: []runtime.Object{
				service("web", func(s *corev1.Service) { s.Spec.ClusterIP = corev1.ClusterIPNone }),
				pod("web-1", webLabels),
				slice("web-abc", "web", ep{"web-1", "10.0.0.1", true}),
			},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 2, wantExit: ExitOK,
			wantChecks: []wantCheck{
				{"Service", "headless", StatusInfo, "clusterIP: None"},
			},
		},
		{
			name: "selectorless service with manual endpoints",
			objects: []runtime.Object{
				service("legacy", withSelector(nil)),
				slice("legacy-1", "legacy", ep{"", "192.168.1.10", true}),
			},
			wantStatus: StatusWarn, wantBroken: -1, wantHops: 2, wantExit: ExitWarning,
			wantChecks: []wantCheck{
				{"Service", "selector", StatusWarn, "no selector"},
				{"EndpointSlices", "endpoints", StatusOK, "1/1"},
			},
		},
		{
			name: "publishNotReadyAddresses is noted",
			objects: []runtime.Object{
				service("web", func(s *corev1.Service) { s.Spec.PublishNotReadyAddresses = true }),
				pod("web-1", webLabels),
				slice("web-abc", "web", ep{"web-1", "10.0.0.1", true}),
			},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 2, wantExit: ExitOK,
			wantChecks: []wantCheck{
				{"EndpointSlices", "publishNotReadyAddresses", StatusInfo, "not ready"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &Tracer{Client: fake.NewClientset(tc.objects...)}
			name := "web"
			if len(tc.objects) > 0 {
				if s, ok := tc.objects[0].(*corev1.Service); ok {
					name = s.Name
				}
			}
			r, err := tr.Trace(context.Background(), Target{Kind: KindService, Namespace: ns, Name: name})
			if err != nil {
				t.Fatalf("Trace: %v", err)
			}
			if r.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s\n%s", r.Status, tc.wantStatus, dump(r))
			}
			if r.FirstBroken != tc.wantBroken {
				t.Errorf("firstBroken = %d, want %d", r.FirstBroken, tc.wantBroken)
			}
			if len(r.Hops) != tc.wantHops {
				t.Fatalf("hops = %d, want %d\n%s", len(r.Hops), tc.wantHops, dump(r))
			}
			if got := r.ExitCode(); got != tc.wantExit {
				t.Errorf("exit = %d, want %d", got, tc.wantExit)
			}
			for _, w := range tc.wantChecks {
				c := findCheck(r, w.hop, w.name)
				if c == nil {
					t.Errorf("check %s/%s missing\n%s", w.hop, w.name, dump(r))
					continue
				}
				if c.Status != w.status {
					t.Errorf("check %s/%s status = %s, want %s", w.hop, w.name, c.Status, w.status)
				}
				text := c.Message + " " + strings.Join(c.Details, " ")
				for _, part := range strings.Fields(w.contains) {
					if !strings.Contains(text, strings.TrimSuffix(part, ";")) {
						t.Errorf("check %s/%s text %q does not contain %q", w.hop, w.name, text, part)
					}
				}
			}
			if tc.wantCauseIn != "" {
				c := r.PrimaryIssue()
				if c == nil || !strings.Contains(c.Cause, tc.wantCauseIn) {
					t.Errorf("primary cause = %+v, want it to contain %q", c, tc.wantCauseIn)
				}
				if len(c.Try) == 0 {
					t.Errorf("primary issue has no Try commands")
				}
			}
		})
	}
}

func TestTraceUnsupportedKinds(t *testing.T) {
	tr := &Tracer{Client: fake.NewClientset()}
	for _, k := range []Kind{KindIngress, KindDeployment} {
		if _, err := tr.Trace(context.Background(), Target{Kind: k, Namespace: ns, Name: "x"}); err == nil {
			t.Errorf("Trace(%s) succeeded, want not-implemented error", k)
		}
	}
}

func findCheck(r *Report, hop, name string) *Check {
	for hi := range r.Hops {
		if r.Hops[hi].Kind != hop {
			continue
		}
		for ci := range r.Hops[hi].Checks {
			if r.Hops[hi].Checks[ci].Name == name {
				return &r.Hops[hi].Checks[ci]
			}
		}
	}
	return nil
}

func dump(r *Report) string {
	var b strings.Builder
	for _, h := range r.Hops {
		b.WriteString(h.Ref() + " [" + h.Status.String() + "]\n")
		for _, c := range h.Checks {
			b.WriteString("  " + c.Name + " [" + c.Status.String() + "] " + c.Message + " " + strings.Join(c.Details, "; ") + "\n")
		}
	}
	return b.String()
}
