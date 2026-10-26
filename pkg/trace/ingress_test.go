package trace

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// --- fixtures ---------------------------------------------------------------

type ingOpt func(*networkingv1.Ingress)

func ingressPath(path string, pt networkingv1.PathType, svc string, port networkingv1.ServiceBackendPort) networkingv1.HTTPIngressPath {
	return networkingv1.HTTPIngressPath{
		Path:     path,
		PathType: &pt,
		Backend:  networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: svc, Port: port}},
	}
}

func rule(host string, paths ...networkingv1.HTTPIngressPath) networkingv1.IngressRule {
	return networkingv1.IngressRule{
		Host:             host,
		IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: paths}},
	}
}

var (
	httpPort = networkingv1.ServiceBackendPort{Name: "http"}
	prefix   = networkingv1.PathTypePrefix
	exact    = networkingv1.PathTypeExact
)

// ingress returns a healthy Ingress: class nginx, shop.example.com/ → svc/web:http.
func ingress(opts ...ingOpt) *networkingv1.Ingress {
	class := "nginx"
	i := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: ns},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &class,
			Rules:            []networkingv1.IngressRule{rule("shop.example.com", ingressPath("/", prefix, "web", httpPort))},
		},
		Status: networkingv1.IngressStatus{LoadBalancer: networkingv1.IngressLoadBalancerStatus{
			Ingress: []networkingv1.IngressLoadBalancerIngress{{IP: "203.0.113.10"}},
		}},
	}
	for _, o := range opts {
		o(i)
	}
	return i
}

func withClass(name string) ingOpt {
	return func(i *networkingv1.Ingress) {
		if name == "" {
			i.Spec.IngressClassName = nil
			return
		}
		i.Spec.IngressClassName = &name
	}
}

func withRules(rules ...networkingv1.IngressRule) ingOpt {
	return func(i *networkingv1.Ingress) { i.Spec.Rules = rules }
}

func withTLS(secret string, hosts ...string) ingOpt {
	return func(i *networkingv1.Ingress) {
		i.Spec.TLS = append(i.Spec.TLS, networkingv1.IngressTLS{Hosts: hosts, SecretName: secret})
	}
}

func ingressClass(name, controller string, isDefault bool) *networkingv1.IngressClass {
	ic := &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       networkingv1.IngressClassSpec{Controller: controller},
	}
	if isDefault {
		ic.Annotations = map[string]string{annotationDefaultClass: "true"}
	}
	return ic
}

func controllerPod(name string, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ingress-nginx",
			Labels: map[string]string{"app.kubernetes.io/name": "ingress-nginx"},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

func tlsSecret(t *testing.T, name string, hosts []string, notBefore, notAfter time.Time) *corev1.Secret {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: hosts[0]},
		DNSNames:     hosts,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			corev1.TLSPrivateKeyKey: []byte("unused"),
		},
	}
}

// healthyBackend is a working svc/web with one ready pod.
func healthyBackend() []runtime.Object {
	return []runtime.Object{
		service("web"),
		pod("web-1", webLabels),
		slice("web-abc", "web", ep{"web-1", "10.0.0.1", true}),
	}
}

func healthyCluster(extra ...runtime.Object) []runtime.Object {
	objs := []runtime.Object{
		ingressClass("nginx", "k8s.io/ingress-nginx", false),
		controllerPod("ingress-nginx-controller-1", true),
	}
	objs = append(objs, healthyBackend()...)
	return append(objs, extra...)
}

// --- tests ------------------------------------------------------------------

type check struct {
	hop, name string
	status    Status
	contains  string
}

func TestTraceIngress(t *testing.T) {
	day := 24 * time.Hour
	tests := []struct {
		name       string
		objects    []runtime.Object
		opts       Options
		wantStatus Status
		wantBroken int
		wantHops   int
		wantChecks []check
		wantCause  string
	}{
		{
			name:       "healthy path",
			objects:    healthyCluster(ingress()),
			opts:       Options{Host: "shop.example.com", Path: "/cart"},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
			wantChecks: []check{
				{"Ingress", "ingressClass", StatusOK, "IngressClass nginx (controller k8s.io/ingress-nginx)"},
				{"Ingress", "controller", StatusOK, "1/1 ingress-nginx pods ready in ingress-nginx"},
				{"Ingress", "address", StatusOK, "203.0.113.10"},
				{"Ingress", "rule", StatusOK, "shop.example.com/cart matches path / (Prefix) → svc/web:http"},
				{"Ingress", "backend", StatusOK, "Service web exposes port http"},
				{"Service", "selector", StatusOK, ""},
				{"EndpointSlices", "endpoints", StatusOK, "1/1"},
			},
		},
		{
			name:       "ingress missing",
			objects:    healthyCluster(),
			wantStatus: StatusFail, wantBroken: 0, wantHops: 1,
			wantChecks: []check{{"Ingress", "exists", StatusFail, `"shop" not found`}},
			wantCause:  "does not exist",
		},
		{
			name:       "IngressClass missing",
			objects:    healthyCluster(ingress(withClass("nginx-internal"))),
			wantStatus: StatusFail, wantBroken: 0, wantHops: 3,
			wantChecks: []check{{"Ingress", "ingressClass", StatusFail, `"nginx-internal" does not exist`}},
			wantCause:  "No controller watches this class",
		},
		{
			name: "controller not running",
			objects: append([]runtime.Object{ingress(), ingressClass("nginx", "k8s.io/ingress-nginx", false)},
				healthyBackend()...),
			wantStatus: StatusFail, wantBroken: 0, wantHops: 3,
			wantChecks: []check{{"Ingress", "controller", StatusFail, "no ingress-nginx pods found"}},
		},
		{
			name: "controller not ready",
			objects: append([]runtime.Object{
				ingress(), ingressClass("nginx", "k8s.io/ingress-nginx", false),
				controllerPod("c-1", false), controllerPod("c-2", false),
			}, healthyBackend()...),
			wantStatus: StatusFail, wantBroken: 0, wantHops: 3,
			wantChecks: []check{{"Ingress", "controller", StatusFail, "0/2 ingress-nginx pods ready"}},
			wantCause:  "controller itself is down",
		},
		{
			name: "controller partly ready",
			objects: append([]runtime.Object{
				ingress(), ingressClass("nginx", "k8s.io/ingress-nginx", false),
				controllerPod("c-1", true), controllerPod("c-2", false),
			}, healthyBackend()...),
			wantStatus: StatusWarn, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "controller", StatusWarn, "1/2"}},
		},
		{
			name:       "no class and no default",
			objects:    healthyCluster(ingress(withClass(""))),
			wantStatus: StatusWarn, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "ingressClass", StatusWarn, "no IngressClass is marked default"}},
		},
		{
			name: "no class uses the default",
			objects: append([]runtime.Object{
				ingress(withClass("")), ingressClass("nginx", "k8s.io/ingress-nginx", true), controllerPod("c-1", true),
			}, healthyBackend()...),
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
			wantChecks: []check{
				{"Ingress", "ingressClass", StatusOK, "default IngressClass nginx applies"},
				{"Ingress", "controller", StatusOK, "1/1"},
			},
		},
		{
			name: "two default classes",
			objects: append([]runtime.Object{
				ingress(withClass("")), ingressClass("a", "k8s.io/ingress-nginx", true), ingressClass("b", "k8s.io/ingress-nginx", true),
			}, healthyBackend()...),
			wantStatus: StatusWarn, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "ingressClass", StatusWarn, "2 IngressClasses are marked default"}},
		},
		{
			name: "legacy annotation",
			objects: append([]runtime.Object{
				ingress(withClass(""), func(i *networkingv1.Ingress) {
					i.Annotations = map[string]string{annotationIngressClass: "nginx"}
				}),
				controllerPod("c-1", true),
			}, healthyBackend()...),
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
			wantChecks: []check{
				{"Ingress", "ingressClass", StatusInfo, "deprecated"},
				{"Ingress", "controller", StatusOK, "1/1"},
			},
		},
		{
			name: "managed GKE controller",
			objects: append([]runtime.Object{
				ingress(withClass("gce")), ingressClass("gce", "k8s.io/ingress-gce", false),
			}, healthyBackend()...),
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "controller", StatusInfo, "managed load balancer"}},
		},
		{
			name: "unknown controller",
			objects: append([]runtime.Object{
				ingress(withClass("x")), ingressClass("x", "example.com/custom", false),
			}, healthyBackend()...),
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "controller", StatusInfo, "pods not checked"}},
		},
		{
			name: "no address yet",
			objects: healthyCluster(ingress(func(i *networkingv1.Ingress) {
				i.Status = networkingv1.IngressStatus{}
			})),
			wantStatus: StatusWarn, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "address", StatusWarn, "no controller has admitted"}},
		},
		{
			name: "wildcard host beats catch-all",
			objects: healthyCluster(ingress(withRules(
				rule("", ingressPath("/", prefix, "other", httpPort)),
				rule("*.example.com", ingressPath("/", prefix, "web", httpPort)),
			))),
			opts:       Options{Host: "api.example.com"},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "rule", StatusOK, "→ svc/web:http"}},
		},
		{
			name: "wildcard covers one label only",
			objects: healthyCluster(ingress(withRules(
				rule("*.example.com", ingressPath("/", prefix, "web", httpPort)),
			))),
			opts:       Options{Host: "a.b.example.com"},
			wantStatus: StatusFail, wantBroken: 0, wantHops: 1,
			wantChecks: []check{{"Ingress", "rule", StatusFail, "no rule matches a.b.example.com/ *.example.com/"}},
			wantCause:  "404",
		},
		{
			name: "longest prefix wins, by path element",
			objects: healthyCluster(ingress(withRules(rule("shop.example.com",
				ingressPath("/", prefix, "other", httpPort),
				ingressPath("/api", prefix, "web", httpPort),
			)))),
			opts:       Options{Host: "shop.example.com", Path: "/api/v1/items"},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "rule", StatusOK, "matches path /api (Prefix) → svc/web:http"}},
		},
		{
			name: "prefix does not match a partial element",
			objects: healthyCluster(ingress(withRules(rule("shop.example.com",
				ingressPath("/", prefix, "web", httpPort),
				ingressPath("/api", prefix, "other", httpPort),
			)))),
			opts:       Options{Host: "shop.example.com", Path: "/apix"},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "rule", StatusOK, "matches path / (Prefix) → svc/web:http"}},
		},
		{
			name: "exact beats prefix of same length",
			objects: healthyCluster(ingress(withRules(rule("shop.example.com",
				ingressPath("/login", prefix, "other", httpPort),
				ingressPath("/login", exact, "web", httpPort),
			)))),
			opts:       Options{Host: "shop.example.com", Path: "/login"},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "rule", StatusOK, "(Exact) → svc/web:http"}},
		},
		{
			name:       "unknown host",
			objects:    healthyCluster(ingress()),
			opts:       Options{Host: "other.example.com"},
			wantStatus: StatusFail, wantBroken: 0, wantHops: 1,
			wantChecks: []check{{"Ingress", "rule", StatusFail, "no rule matches other.example.com/ shop.example.com/ (Prefix)"}},
		},
		{
			name: "defaultBackend catches the rest",
			objects: healthyCluster(ingress(func(i *networkingv1.Ingress) {
				i.Spec.DefaultBackend = &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "web", Port: httpPort}}
			})),
			opts:       Options{Host: "other.example.com"},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "rule", StatusInfo, "spec.defaultBackend → svc/web:http"}},
		},
		{
			name: "several hosts without --host",
			objects: healthyCluster(ingress(withRules(
				rule("shop.example.com", ingressPath("/", prefix, "web", httpPort)),
				rule("admin.example.com", ingressPath("/", prefix, "other", httpPort)),
			))),
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
			wantChecks: []check{
				{"Ingress", "host", StatusInfo, "serves 2 hosts; tracing shop.example.com"},
				{"Ingress", "rule", StatusOK, "svc/web"},
			},
		},
		{
			name: "backend Service missing",
			objects: healthyCluster(ingress(withRules(rule("shop.example.com",
				ingressPath("/", prefix, "gone", httpPort),
			)))),
			wantStatus: StatusFail, wantBroken: 0, wantHops: 1,
			wantChecks: []check{{"Ingress", "backend", StatusFail, `backend Service "gone" not found`}},
			wantCause:  "answers 503",
		},
		{
			name: "backend port missing",
			objects: healthyCluster(ingress(withRules(rule("shop.example.com",
				ingressPath("/", prefix, "web", networkingv1.ServiceBackendPort{Number: 8080}),
			)))),
			wantStatus: StatusFail, wantBroken: 0, wantHops: 3,
			wantChecks: []check{{"Ingress", "backend", StatusFail, "Service web has no port 8080 service ports: http=80"}},
			wantCause:  "not the targetPort",
		},
		{
			name: "backend port by number",
			objects: healthyCluster(ingress(withRules(rule("shop.example.com",
				ingressPath("/", prefix, "web", networkingv1.ServiceBackendPort{Number: 80}),
			)))),
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "backend", StatusOK, "exposes port 80"}},
		},
		{
			name: "resource backend",
			objects: healthyCluster(ingress(withRules(rule("shop.example.com", networkingv1.HTTPIngressPath{
				Path: "/", PathType: &prefix,
				Backend: networkingv1.IngressBackend{Resource: &corev1.TypedLocalObjectReference{Kind: "StorageBucket", Name: "assets"}},
			})))),
			wantStatus: StatusOK, wantBroken: -1, wantHops: 1,
			wantChecks: []check{{"Ingress", "backend", StatusInfo, "resource reference"}},
		},
		{
			name: "broken hop downstream of the Ingress",
			objects: []runtime.Object{
				ingress(), ingressClass("nginx", "k8s.io/ingress-nginx", false), controllerPod("c-1", true),
				service("web"), pod("web-1", webLabels), slice("web-abc", "web", ep{"web-1", "10.0.0.1", false}),
			},
			wantStatus: StatusFail, wantBroken: 2, wantHops: 3,
			wantChecks: []check{{"EndpointSlices", "endpoints", StatusFail, "0/1"}},
			wantCause:  "readiness",
		},
		{
			name:       "TLS valid",
			objects:    healthyCluster(ingress(withTLS("shop-tls", "shop.example.com")), tlsSecret(t, "shop-tls", []string{"shop.example.com"}, now.Add(-day), now.Add(60*day))),
			opts:       Options{CheckTLS: true},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "tls", StatusOK, "valid until 2026-12-25 (60 days)"}},
		},
		{
			name:       "TLS wildcard certificate",
			objects:    healthyCluster(ingress(withTLS("wild", "*.example.com")), tlsSecret(t, "wild", []string{"*.example.com"}, now.Add(-day), now.Add(60*day))),
			opts:       Options{CheckTLS: true},
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "tls", StatusOK, "certificate for shop.example.com"}},
		},
		{
			name:       "TLS expired",
			objects:    healthyCluster(ingress(withTLS("shop-tls", "shop.example.com")), tlsSecret(t, "shop-tls", []string{"shop.example.com"}, now.Add(-90*day), now.Add(-day))),
			opts:       Options{CheckTLS: true},
			wantStatus: StatusFail, wantBroken: 0, wantHops: 3,
			wantChecks: []check{{"Ingress", "tls", StatusFail, "expired on 2026-10-25"}},
			wantCause:  "expired",
		},
		{
			name:       "TLS not yet valid",
			objects:    healthyCluster(ingress(withTLS("shop-tls", "shop.example.com")), tlsSecret(t, "shop-tls", []string{"shop.example.com"}, now.Add(day), now.Add(90*day))),
			opts:       Options{CheckTLS: true},
			wantStatus: StatusFail, wantBroken: 0, wantHops: 3,
			wantChecks: []check{{"Ingress", "tls", StatusFail, "not valid until"}},
		},
		{
			name:       "TLS SAN mismatch",
			objects:    healthyCluster(ingress(withTLS("shop-tls", "shop.example.com")), tlsSecret(t, "shop-tls", []string{"www.example.com"}, now.Add(-day), now.Add(60*day))),
			opts:       Options{CheckTLS: true},
			wantStatus: StatusFail, wantBroken: 0, wantHops: 3,
			wantChecks: []check{{"Ingress", "tls", StatusFail, "does not cover shop.example.com SANs: www.example.com"}},
			wantCause:  "name mismatch",
		},
		{
			name:       "TLS expiring soon",
			objects:    healthyCluster(ingress(withTLS("shop-tls", "shop.example.com")), tlsSecret(t, "shop-tls", []string{"shop.example.com"}, now.Add(-day), now.Add(5*day))),
			opts:       Options{CheckTLS: true},
			wantStatus: StatusWarn, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "tls", StatusWarn, "expires in 5 day(s)"}},
		},
		{
			name:       "TLS secret missing",
			objects:    healthyCluster(ingress(withTLS("shop-tls", "shop.example.com"))),
			opts:       Options{CheckTLS: true},
			wantStatus: StatusFail, wantBroken: 0, wantHops: 3,
			wantChecks: []check{{"Ingress", "tls", StatusFail, `TLS secret "shop-tls" not found`}},
			wantCause:  "cert-manager",
		},
		{
			name: "TLS secret without a certificate",
			objects: healthyCluster(ingress(withTLS("shop-tls", "shop.example.com")), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "shop-tls", Namespace: ns},
				Data:       map[string][]byte{corev1.TLSCertKey: []byte("not pem")},
			}),
			opts:       Options{CheckTLS: true},
			wantStatus: StatusFail, wantBroken: 0, wantHops: 3,
			wantChecks: []check{{"Ingress", "tls", StatusFail, "holds no PEM certificate"}},
		},
		{
			name:       "TLS entry missing for host",
			objects:    healthyCluster(ingress(withTLS("other-tls", "other.example.com"))),
			opts:       Options{CheckTLS: true},
			wantStatus: StatusWarn, wantBroken: -1, wantHops: 3,
			wantChecks: []check{{"Ingress", "tls", StatusWarn, "no TLS entry covers shop.example.com"}},
		},
		{
			name:       "TLS ignored without --check-tls",
			objects:    healthyCluster(ingress(withTLS("shop-tls", "shop.example.com"))),
			wantStatus: StatusOK, wantBroken: -1, wantHops: 3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &Tracer{Client: fake.NewClientset(tc.objects...), Options: tc.opts, Now: func() time.Time { return now }}
			r, err := tr.Trace(context.Background(), Target{Kind: KindIngress, Namespace: ns, Name: "shop"})
			if err != nil {
				t.Fatalf("Trace: %v", err)
			}
			if r.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s\n%s", r.Status, tc.wantStatus, dump(r))
			}
			if r.FirstBroken != tc.wantBroken {
				t.Errorf("firstBroken = %d, want %d\n%s", r.FirstBroken, tc.wantBroken, dump(r))
			}
			if len(r.Hops) != tc.wantHops {
				t.Fatalf("hops = %d, want %d\n%s", len(r.Hops), tc.wantHops, dump(r))
			}
			for _, w := range tc.wantChecks {
				assertCheck(t, r, w)
			}
			if !tc.opts.CheckTLS && findCheck(r, "Ingress", "tls") != nil {
				t.Error("tls check ran without --check-tls")
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

func assertCheck(t *testing.T, r *Report, w check) {
	t.Helper()
	c := findCheck(r, w.hop, w.name)
	if c == nil {
		t.Errorf("check %s/%s missing\n%s", w.hop, w.name, dump(r))
		return
	}
	if c.Status != w.status {
		t.Errorf("check %s/%s status = %s, want %s\n%s", w.hop, w.name, c.Status, w.status, dump(r))
	}
	text := c.Message + " " + strings.Join(c.Details, " ")
	for _, part := range strings.Fields(w.contains) {
		if !strings.Contains(text, part) {
			t.Errorf("check %s/%s text %q does not contain %q", w.hop, w.name, text, part)
		}
	}
}

func TestWildcardMatch(t *testing.T) {
	tests := []struct {
		pattern, host string
		want          bool
	}{
		{"*.example.com", "a.example.com", true},
		{"*.example.com", "example.com", false},
		{"*.example.com", "a.b.example.com", false},
		{"*.example.com", ".example.com", false},
		{"example.com", "example.com", false},
		{"*example.com", "aexample.com", false},
	}
	for _, tc := range tests {
		if got := wildcardMatch(tc.pattern, tc.host); got != tc.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", tc.pattern, tc.host, got, tc.want)
		}
	}
}
