package trace

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// --- fixtures ---------------------------------------------------------------

func waiting(reason, msg string) podOpt {
	return func(p *corev1.Pod) {
		notReady(p)
		p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}
	}
}

func lastExit(reason string, code int32, restarts int32, ago time.Duration) podOpt {
	return func(p *corev1.Pod) {
		cs := &p.Status.ContainerStatuses[0]
		cs.RestartCount = restarts
		cs.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: reason, ExitCode: code, FinishedAt: metav1.NewTime(now.Add(-ago)),
		}}
	}
}

func onNode(name string) podOpt {
	return func(p *corev1.Pod) { p.Spec.NodeName = name }
}

func node(name string, ready corev1.ConditionStatus, reason string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: ready, Reason: reason},
		}},
	}
}

func event(podName, reason, msg string, count int32) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: fmt.Sprintf("%s.%s", podName, strings.ToLower(reason)), Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: podName},
		Reason:         reason,
		Message:        msg,
		Count:          count,
		LastTimestamp:  metav1.NewTime(now.Add(-time.Minute)),
	}
}

func deployment(name string, replicas, ready int32, opts ...func(*appsv1.Deployment)) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name, "tier": "web"}}},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: ready, UpdatedReplicas: replicas},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

func runTrace(t *testing.T, objects []runtime.Object, target Target) *Report {
	t.Helper()
	tr := &Tracer{Client: fake.NewClientset(objects...), Now: func() time.Time { return now }}
	r, err := tr.Trace(context.Background(), target)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	return r
}

// --- tests ------------------------------------------------------------------

func TestPodsHop(t *testing.T) {
	svcTarget := Target{Kind: KindService, Namespace: ns, Name: "web"}
	readySlice := slice("web-abc", "web", ep{"web-1", "10.0.0.1", true})
	notReadySlice := slice("web-abc", "web", ep{"web-1", "10.0.0.1", false})

	tests := []struct {
		name       string
		objects    []runtime.Object
		wantStatus Status
		wantChecks []check
		wantCause  string
		// wantNoPodChecks asserts the Pods hop lists no individual pod.
		wantNoPodChecks bool
	}{
		{
			name:            "healthy",
			objects:         []runtime.Object{service("web"), pod("web-1", webLabels), readySlice},
			wantStatus:      StatusOK,
			wantChecks:      []check{{"Pods", "ready", StatusOK, "1/1 pod(s) ready"}},
			wantNoPodChecks: true,
		},
		{
			name: "unschedulable",
			objects: []runtime.Object{service("web"), pod("web-1", webLabels, notReady, func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
				p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
					Message: "0/3 nodes are available: 3 Insufficient cpu.",
				})
			})},
			wantStatus: StatusFail,
			wantChecks: []check{
				{"Pods", "ready", StatusFail, "0/1"},
				{"Pods", "pod web-1", StatusFail, "Pending: Unschedulable 3 Insufficient cpu."},
			},
			wantCause: "scheduler cannot place the pod",
		},
		{
			name:       "image pull failure",
			objects:    []runtime.Object{service("web"), pod("web-1", webLabels, waiting("ImagePullBackOff", `Back-off pulling image "web:1.0"`))},
			wantStatus: StatusFail,
			wantChecks: []check{{"Pods", "pod web-1", StatusFail, "container app: ImagePullBackOff (web:1.0)"}},
			wantCause:  "image cannot be pulled",
		},
		{
			name:       "missing config",
			objects:    []runtime.Object{service("web"), pod("web-1", webLabels, waiting("CreateContainerConfigError", `secret "db" not found`))},
			wantStatus: StatusFail,
			wantChecks: []check{{"Pods", "pod web-1", StatusFail, `CreateContainerConfigError secret "db" not found`}},
			wantCause:  "ConfigMap, Secret or key",
		},
		{
			name: "crash loop after OOM",
			objects: []runtime.Object{service("web"), notReadySlice,
				pod("web-1", webLabels, waiting("CrashLoopBackOff", ""), lastExit("OOMKilled", 137, 7, time.Minute))},
			wantStatus: StatusFail,
			wantChecks: []check{{"Pods", "pod web-1", StatusFail, "CrashLoopBackOff, 7 restart(s) last exit: OOMKilled, code 137"}},
			wantCause:  "exceeding its memory limit",
		},
		{
			name: "crash loop with an error exit",
			objects: []runtime.Object{service("web"), notReadySlice,
				pod("web-1", webLabels, waiting("CrashLoopBackOff", ""), lastExit("Error", 1, 4, time.Minute))},
			wantStatus: StatusFail,
			wantChecks: []check{{"Pods", "pod web-1", StatusFail, "last exit: Error, code 1"}},
			wantCause:  "previous logs",
		},
		{
			name: "node not ready",
			objects: []runtime.Object{service("web"), notReadySlice,
				pod("web-1", webLabels, notReady, onNode("n1")), node("n1", corev1.ConditionUnknown, "NodeStatusUnknown")},
			wantStatus: StatusFail,
			wantChecks: []check{{"Pods", "pod web-1", StatusFail, "node n1 is NotReady (NodeStatusUnknown)"}},
			wantCause:  "node is unhealthy",
		},
		{
			name: "healthy node is fine",
			objects: []runtime.Object{service("web"), readySlice,
				pod("web-1", webLabels, onNode("n1")), node("n1", corev1.ConditionTrue, "KubeletReady")},
			wantStatus:      StatusOK,
			wantNoPodChecks: true,
		},
		{
			name: "readiness probe failing",
			objects: []runtime.Object{service("web"), notReadySlice, pod("web-1", webLabels, notReady),
				event("web-1", "Unhealthy", "Readiness probe failed: HTTP probe failed with statuscode: 503", 12),
				event("web-1", "Pulled", "Container image already present", 1),
				event("other", "Unhealthy", "Readiness probe failed: other pod", 1),
			},
			wantStatus: StatusFail,
			wantChecks: []check{
				{"EndpointSlices", "endpoints", StatusFail, "0/1"},
				{"Pods", "pod web-1", StatusFail, "readiness probe failing statuscode: 503 (x12)"},
			},
			wantCause: "readiness probe fails",
		},
		{
			name: "one broken pod among ready ones only degrades",
			objects: []runtime.Object{service("web"),
				pod("web-1", webLabels),
				pod("web-2", webLabels, waiting("CrashLoopBackOff", ""), lastExit("OOMKilled", 137, 6, time.Minute)),
				slice("web-abc", "web", ep{"web-1", "10.0.0.1", true}, ep{"web-2", "10.0.0.2", false}),
			},
			wantStatus: StatusWarn,
			wantChecks: []check{
				{"EndpointSlices", "endpoints", StatusWarn, "1/2"},
				{"Pods", "ready", StatusWarn, "1/2"},
				{"Pods", "pod web-2", StatusWarn, "CrashLoopBackOff"},
			},
			// The endpoints warning comes first and carries the pod's diagnosis.
			wantCause: "exceeding its memory limit",
		},
		{
			name:       "not ready without events",
			objects:    []runtime.Object{service("web"), notReadySlice, pod("web-1", webLabels, notReady)},
			wantStatus: StatusFail,
			wantChecks: []check{{"Pods", "pod web-1", StatusFail, "not ready: container app"}},
			wantCause:  "no probe failure events",
		},
		{
			name: "still starting",
			objects: []runtime.Object{service("web"), pod("web-1", webLabels, waiting("ContainerCreating", ""), func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
			})},
			wantStatus: StatusFail, // no endpoints yet
			wantChecks: []check{{"Pods", "pod web-1", StatusWarn, "Pending: ContainerCreating"}},
		},
		{
			name: "recent OOM restart on a ready pod",
			objects: []runtime.Object{service("web"), readySlice,
				pod("web-1", webLabels, lastExit("OOMKilled", 137, 3, 2*time.Hour)),
				event("web-1", "Unhealthy", "Liveness probe failed: connection refused", 3),
			},
			wantStatus: StatusWarn,
			wantChecks: []check{
				{"Pods", "ready", StatusOK, "1/1"},
				{"Pods", "pod web-1", StatusWarn, "restarted 3 time(s), last 2h ago OOMKilled, code 137 Liveness probe failed: connection refused (x3)"},
			},
			wantCause: "OOMKilled",
		},
		{
			name: "SIGKILL restart is explained",
			objects: []runtime.Object{service("web"), readySlice,
				pod("web-1", webLabels, lastExit("Error", 137, 1, 6*time.Hour)),
			},
			wantStatus: StatusWarn,
			wantChecks: []check{{"Pods", "pod web-1", StatusWarn, "last 6h ago Error, code 137"}},
			wantCause:  "sent SIGKILL",
		},
		{
			name: "crash loop with SIGKILL",
			objects: []runtime.Object{service("web"), notReadySlice,
				pod("web-1", webLabels, waiting("CrashLoopBackOff", ""), lastExit("Error", 137, 9, time.Minute))},
			wantStatus: StatusFail,
			wantChecks: []check{{"Pods", "pod web-1", StatusFail, "code 137"}},
			wantCause:  "failed liveness probe",
		},
		{
			name:            "old restart is not news",
			objects:         []runtime.Object{service("web"), readySlice, pod("web-1", webLabels, lastExit("Error", 1, 1, 72*time.Hour))},
			wantStatus:      StatusOK,
			wantNoPodChecks: true,
		},
		{
			name: "terminating pod is a note",
			objects: []runtime.Object{service("web"), readySlice, pod("web-1", webLabels, func(p *corev1.Pod) {
				ts := metav1.NewTime(now)
				p.DeletionTimestamp = &ts
				p.Finalizers = []string{"test"}
			})},
			wantStatus: StatusOK,
			wantChecks: []check{{"Pods", "pod web-1", StatusInfo, "terminating"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := runTrace(t, tc.objects, svcTarget)
			if r.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s\n%s", r.Status, tc.wantStatus, dump(r))
			}
			if len(r.Hops) != 3 || r.Hops[2].Kind != "Pods" {
				t.Fatalf("want Service, EndpointSlices, Pods hops\n%s", dump(r))
			}
			for _, w := range tc.wantChecks {
				assertCheck(t, r, w)
			}
			if tc.wantNoPodChecks && len(r.Hops[2].Checks) != 1 {
				t.Errorf("want only the summary check in the Pods hop\n%s", dump(r))
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

func TestPodsHopOrderingAndCap(t *testing.T) {
	objs := []runtime.Object{service("web")}
	// No pod is ready, so failures stay failures. a-* only warns (still
	// starting), z-* fail; failures must be listed first.
	objs = append(objs, pod("a-starting", webLabels, waiting("ContainerCreating", ""), func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodPending
	}))
	for i := 0; i < 6; i++ {
		objs = append(objs, pod(fmt.Sprintf("z-crash-%d", i), webLabels, waiting("CrashLoopBackOff", ""), lastExit("Error", 1, 5, time.Minute)))
	}
	r := runTrace(t, objs, Target{Kind: KindService, Namespace: ns, Name: "web"})
	checks := r.Hops[2].Checks
	if len(checks) != 1+maxPodChecks+1 {
		t.Fatalf("got %d checks, want summary + %d pods + more\n%s", len(checks), maxPodChecks, dump(r))
	}
	if checks[1].Name != "pod z-crash-0" || checks[1].Status != StatusFail {
		t.Errorf("first pod check = %s (%s), want a failing pod first", checks[1].Name, checks[1].Status)
	}
	if last := checks[len(checks)-1]; last.Name != "more" || !strings.Contains(last.Message, "2 more") {
		t.Errorf("last check = %+v, want '2 more'", last)
	}
	if checks[0].Message != "0/7 pod(s) ready" {
		t.Errorf("summary = %q", checks[0].Message)
	}
}

func TestTraceDeployment(t *testing.T) {
	webSvc := service("web", withSelector(map[string]string{"app": "web"}))
	webPod := pod("web-1", map[string]string{"app": "web", "tier": "web"})
	webSlice := slice("web-abc", "web", ep{"web-1", "10.0.0.1", true})
	backend := []runtime.Object{webSvc, webPod, webSlice}

	tests := []struct {
		name       string
		objects    []runtime.Object
		wantStatus Status
		wantKinds  string
		wantChecks []check
		wantCause  string
	}{
		{
			name:       "healthy",
			objects:    append([]runtime.Object{deployment("web", 1, 1)}, backend...),
			wantStatus: StatusOK,
			wantKinds:  "Deployment Service EndpointSlices Pods",
			wantChecks: []check{
				{"Deployment", "replicas", StatusOK, "1/1 replicas ready"},
				{"Deployment", "services", StatusOK, "selected by svc/web"},
			},
		},
		{
			name:       "missing",
			objects:    backend,
			wantStatus: StatusFail,
			wantKinds:  "Deployment",
			wantChecks: []check{{"Deployment", "exists", StatusFail, `"web" not found`}},
			wantCause:  "does not exist",
		},
		{
			name:       "scaled to zero",
			objects:    []runtime.Object{deployment("web", 0, 0), webSvc},
			wantStatus: StatusFail, // and the Service has no endpoints
			wantKinds:  "Deployment Service EndpointSlices",
			wantChecks: []check{{"Deployment", "replicas", StatusWarn, "scaled to 0"}},
		},
		{
			name:       "no ready replicas",
			objects:    append([]runtime.Object{deployment("web", 2, 0)}, backend...),
			wantStatus: StatusFail,
			wantKinds:  "Deployment Service EndpointSlices Pods",
			wantChecks: []check{{"Deployment", "replicas", StatusFail, "0/2 replicas ready"}},
			wantCause:  "Pods hop shows why",
		},
		{
			name: "rollout stuck",
			objects: append([]runtime.Object{deployment("web", 1, 1, func(d *appsv1.Deployment) {
				d.Status.Conditions = []appsv1.DeploymentCondition{{
					Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
					Reason: "ProgressDeadlineExceeded", Message: `ReplicaSet "web-2" has timed out progressing.`,
				}}
			})}, backend...),
			wantStatus: StatusFail,
			wantKinds:  "Deployment Service EndpointSlices Pods",
			wantChecks: []check{{"Deployment", "rollout", StatusFail, "progress deadline exceeded timed out"}},
			wantCause:  "stopped making progress",
		},
		{
			name: "replica warning explains the broken pod",
			objects: []runtime.Object{deployment("web", 2, 1), webSvc,
				webPod,
				pod("web-2", map[string]string{"app": "web", "tier": "web"}, waiting("ImagePullBackOff", "")),
				slice("web-abc", "web", ep{"web-1", "10.0.0.1", true}, ep{"web-2", "10.0.0.2", false}),
			},
			wantStatus: StatusWarn,
			wantKinds:  "Deployment Service EndpointSlices Pods",
			wantChecks: []check{
				{"Deployment", "replicas", StatusWarn, "1/2"},
				{"Pods", "pod web-2", StatusWarn, "ImagePullBackOff"},
			},
			wantCause: "image cannot be pulled",
		},
		{
			name: "rollout in progress",
			objects: append([]runtime.Object{deployment("web", 3, 3, func(d *appsv1.Deployment) {
				d.Status.UpdatedReplicas = 1
			})}, backend...),
			wantStatus: StatusOK,
			wantKinds:  "Deployment Service EndpointSlices Pods",
			wantChecks: []check{{"Deployment", "rollout", StatusInfo, "1/3 replicas updated"}},
		},
		{
			name: "no Service selects it",
			objects: []runtime.Object{deployment("web", 1, 1), webPod,
				service("other"),
				service("legacy", withSelector(nil)),
				service("ext", func(s *corev1.Service) {
					s.Spec = corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "x.example.com"}
				}),
			},
			wantStatus: StatusWarn,
			wantKinds:  "Deployment Pods",
			wantChecks: []check{
				{"Deployment", "services", StatusWarn, "no Service selects"},
				{"Pods", "ready", StatusOK, "1/1"},
			},
			wantCause: "Nothing routes traffic",
		},
		{
			name: "two Services and an Ingress",
			objects: append([]runtime.Object{
				deployment("web", 1, 1),
				service("web-internal", withSelector(map[string]string{"tier": "web"})),
				ingress(),
			}, backend...),
			wantStatus: StatusFail, // web-internal has no EndpointSlices
			wantKinds:  "Deployment Service EndpointSlices Service EndpointSlices Pods",
			wantChecks: []check{
				{"Deployment", "services", StatusOK, "selected by svc/web, svc/web-internal"},
				{"Deployment", "ingresses", StatusInfo, "exposed via ingress/shop (shop.example.com/)"},
			},
		},
		{
			name: "default backend counts as a route",
			objects: append([]runtime.Object{deployment("web", 1, 1), ingress(withRules(), func(i *networkingv1.Ingress) {
				i.Spec.DefaultBackend = &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "web", Port: httpPort}}
			})}, backend...),
			wantStatus: StatusOK,
			wantKinds:  "Deployment Service EndpointSlices Pods",
			wantChecks: []check{{"Deployment", "ingresses", StatusInfo, "ingress/shop (default backend)"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := runTrace(t, tc.objects, Target{Kind: KindDeployment, Namespace: ns, Name: "web"})
			if r.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s\n%s", r.Status, tc.wantStatus, dump(r))
			}
			var kinds []string
			for _, h := range r.Hops {
				kinds = append(kinds, h.Kind)
			}
			if got := strings.Join(kinds, " "); got != tc.wantKinds {
				t.Fatalf("hops = %s, want %s\n%s", got, tc.wantKinds, dump(r))
			}
			for _, w := range tc.wantChecks {
				assertCheck(t, r, w)
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
