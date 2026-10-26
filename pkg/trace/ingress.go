package trace

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	annotationIngressClass = "kubernetes.io/ingress.class"
	annotationDefaultClass = "ingressclass.kubernetes.io/is-default-class"
	// certExpiryWarning is how close to expiry a certificate starts to warn.
	certExpiryWarning = 14 * 24 * time.Hour
)

// controllerInfo says how to find the pods of a known ingress controller.
type controllerInfo struct {
	name string
	// selectors are tried in order, since installs differ (Helm chart vs manifests).
	selectors []string
	// managed controllers run outside the cluster (cloud load balancers).
	managed bool
}

// knownControllers is keyed by IngressClass spec.controller.
var knownControllers = map[string]controllerInfo{
	"k8s.io/ingress-nginx":                   {name: "ingress-nginx", selectors: []string{"app.kubernetes.io/name=ingress-nginx"}},
	"nginx.org/ingress-controller":           {name: "NGINX Ingress Controller", selectors: []string{"app.kubernetes.io/name=nginx-ingress"}},
	"traefik.io/ingress-controller":          {name: "Traefik", selectors: []string{"app.kubernetes.io/name=traefik"}},
	"haproxy.org/ingress-controller/haproxy": {name: "HAProxy", selectors: []string{"app.kubernetes.io/name=kubernetes-ingress"}},
	"ingress.k8s.aws/alb":                    {name: "AWS Load Balancer Controller", selectors: []string{"app.kubernetes.io/name=aws-load-balancer-controller"}},
	"ingress-controllers.konghq.com/kong": {name: "Kong", selectors: []string{
		"app.kubernetes.io/name=kong",
		"app=ingress-kong",
		"app.kubernetes.io/name=controller,app.kubernetes.io/instance=kong",
	}},
	"k8s.io/ingress-gce":            {name: "GKE Ingress", managed: true},
	"networking.gke.io/ingress-gce": {name: "GKE Ingress", managed: true},
	"azure/application-gateway":     {name: "Azure Application Gateway", managed: true},
}

// legacyClassControllers maps common values of the deprecated
// kubernetes.io/ingress.class annotation to a controller.
var legacyClassControllers = map[string]string{
	"nginx":   "k8s.io/ingress-nginx",
	"traefik": "traefik.io/ingress-controller",
	"haproxy": "haproxy.org/ingress-controller/haproxy",
	"alb":     "ingress.k8s.aws/alb",
	"gce":     "k8s.io/ingress-gce",
}

// traceIngress returns the Ingress hop followed by the hops of the backend
// Service that the selected rule points at.
func (t *Tracer) traceIngress(ctx context.Context, ns, name string) ([]Hop, error) {
	hop := Hop{Kind: "Ingress", Namespace: ns, Name: name}

	ing, err := t.Client.NetworkingV1().Ingresses(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		hop.Add(Check{
			Name:    "exists",
			Status:  StatusFail,
			Message: fmt.Sprintf("Ingress %q not found in namespace %q", name, ns),
			Cause:   "The Ingress does not exist (wrong name or namespace, or it was never applied).",
			Try:     []string{fmt.Sprintf("kubectl get ingress -n %s", ns)},
		})
		return []Hop{hop}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get ingress %s/%s: %w", ns, name, err)
	}
	hop.Add(Check{Name: "exists", Status: StatusOK, Message: fmt.Sprintf("%d rule(s), %d TLS section(s)", len(ing.Spec.Rules), len(ing.Spec.TLS))})

	if err := t.checkIngressClass(ctx, ing, &hop); err != nil {
		return nil, err
	}
	checkIngressAddress(ing, &hop)

	host, backend := t.matchRule(ing, &hop)
	if t.Options.CheckTLS {
		if err := t.checkTLS(ctx, ing, host, &hop); err != nil {
			return nil, err
		}
	}
	if backend == nil {
		return []Hop{hop}, nil
	}
	if backend.Service == nil {
		hop.Add(Check{
			Name:    "backend",
			Status:  StatusInfo,
			Message: "backend is a resource reference, not a Service; nothing further to trace",
		})
		return []Hop{hop}, nil
	}

	svcName := backend.Service.Name
	svc, err := t.Client.CoreV1().Services(ns).Get(ctx, svcName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		hop.Add(Check{
			Name:    "backend",
			Status:  StatusFail,
			Message: fmt.Sprintf("backend Service %q not found", svcName),
			Cause:   "The Ingress points at a Service that does not exist, so the controller has no upstream and answers 503.",
			Try: []string{
				fmt.Sprintf("kubectl get svc -n %s", ns),
				fmt.Sprintf("kubectl describe ingress %s -n %s", name, ns),
			},
		})
		return []Hop{hop}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get service %s/%s: %w", ns, svcName, err)
	}
	checkBackendPort(ing, svc, backend.Service.Port, &hop)

	svcHops, err := t.traceService(ctx, ns, svcName)
	if err != nil {
		return nil, err
	}
	return append([]Hop{hop}, svcHops...), nil
}

// checkIngressClass resolves the class of ing and checks that a controller is
// there to serve it.
func (t *Tracer) checkIngressClass(ctx context.Context, ing *networkingv1.Ingress, hop *Hop) error {
	tryClasses := "kubectl get ingressclass"

	if ing.Spec.IngressClassName != nil && *ing.Spec.IngressClassName != "" {
		className := *ing.Spec.IngressClassName
		ic, err := t.Client.NetworkingV1().IngressClasses().Get(ctx, className, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			hop.Add(Check{
				Name:    "ingressClass",
				Status:  StatusFail,
				Message: fmt.Sprintf("IngressClass %q does not exist", className),
				Cause:   "No controller watches this class, so the Ingress is never programmed: the host does not resolve to it, or the controller answers 404.",
				Try:     []string{tryClasses},
			})
			return nil
		case apierrors.IsForbidden(err):
			hop.Add(Check{Name: "ingressClass", Status: StatusInfo, Message: fmt.Sprintf("class %q: not allowed to read IngressClasses, skipped", className)})
			return nil
		case err != nil:
			return fmt.Errorf("get ingressclass %s: %w", className, err)
		}
		hop.Add(Check{Name: "ingressClass", Status: StatusOK, Message: fmt.Sprintf("IngressClass %s (controller %s)", ic.Name, ic.Spec.Controller)})
		return t.checkController(ctx, ic.Spec.Controller, hop)
	}

	if legacy := ing.Annotations[annotationIngressClass]; legacy != "" {
		hop.Add(Check{
			Name:    "ingressClass",
			Status:  StatusInfo,
			Message: fmt.Sprintf("class %q set via the deprecated %s annotation; prefer spec.ingressClassName", legacy, annotationIngressClass),
		})
		if ctrl, ok := legacyClassControllers[legacy]; ok {
			return t.checkController(ctx, ctrl, hop)
		}
		// The annotation may also name an IngressClass object.
		ic, err := t.Client.NetworkingV1().IngressClasses().Get(ctx, legacy, metav1.GetOptions{})
		if err == nil {
			return t.checkController(ctx, ic.Spec.Controller, hop)
		}
		return nil
	}

	list, err := t.Client.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{})
	if apierrors.IsForbidden(err) {
		hop.Add(Check{Name: "ingressClass", Status: StatusInfo, Message: "no class set and not allowed to list IngressClasses, skipped"})
		return nil
	}
	if err != nil {
		return fmt.Errorf("list ingressclasses: %w", err)
	}
	var defaults []networkingv1.IngressClass
	for _, ic := range list.Items {
		if ic.Annotations[annotationDefaultClass] == "true" {
			defaults = append(defaults, ic)
		}
	}
	switch len(defaults) {
	case 0:
		hop.Add(Check{
			Name:    "ingressClass",
			Status:  StatusWarn,
			Message: "no ingressClassName set and no IngressClass is marked default",
			Cause:   "Most controllers ignore an Ingress without a class unless one IngressClass is marked default, so this Ingress may never be served.",
			Try:     []string{tryClasses},
		})
		return nil
	case 1:
		hop.Add(Check{Name: "ingressClass", Status: StatusOK, Message: fmt.Sprintf("no class set; default IngressClass %s applies (controller %s)", defaults[0].Name, defaults[0].Spec.Controller)})
		return t.checkController(ctx, defaults[0].Spec.Controller, hop)
	default:
		hop.Add(Check{
			Name:    "ingressClass",
			Status:  StatusWarn,
			Message: fmt.Sprintf("no class set and %d IngressClasses are marked default", len(defaults)),
			Cause:   "With several default classes it is ambiguous which controller serves this Ingress; set spec.ingressClassName.",
			Try:     []string{tryClasses},
		})
		return nil
	}
}

// checkController looks for running pods of a known in-cluster controller.
func (t *Tracer) checkController(ctx context.Context, controller string, hop *Hop) error {
	info, ok := knownControllers[controller]
	switch {
	case !ok:
		hop.Add(Check{Name: "controller", Status: StatusInfo, Message: fmt.Sprintf("controller %q is not one kubectl-hops knows how to find; pods not checked", controller)})
		return nil
	case info.managed:
		hop.Add(Check{Name: "controller", Status: StatusInfo, Message: fmt.Sprintf("%s is a managed load balancer; no in-cluster controller pods to check", info.name)})
		return nil
	}

	var total, ready int
	namespaces := map[string]bool{}
	var firstNS string
	selector := info.selectors[0]
	for _, sel := range info.selectors {
		list, err := t.Client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if apierrors.IsForbidden(err) {
			hop.Add(Check{Name: "controller", Status: StatusInfo, Message: fmt.Sprintf("not allowed to list pods cluster-wide; %s pods not checked", info.name)})
			return nil
		}
		if err != nil {
			return fmt.Errorf("list %s pods: %w", info.name, err)
		}
		for _, p := range list.Items {
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue // admission jobs and the like
			}
			total++
			namespaces[p.Namespace] = true
			if firstNS == "" {
				firstNS = p.Namespace
			}
			if podReady(&p) {
				ready++
			}
		}
		if total > 0 {
			selector = sel
			break
		}
	}
	try := []string{fmt.Sprintf("kubectl get pods -A -l %s", selector)}

	switch {
	case total == 0:
		// A miss may also be an install with non-standard labels, so only warn.
		hop.Add(Check{
			Name:    "controller",
			Status:  StatusWarn,
			Message: fmt.Sprintf("no %s pods found with the standard labels (%s)", info.name, strings.Join(info.selectors, " | ")),
			Cause:   fmt.Sprintf("Either %s is not running, so nothing serves this IngressClass, or it was installed with non-standard labels.", info.name),
			Try:     try,
		})
	case ready == 0:
		hop.Add(Check{
			Name:    "controller",
			Status:  StatusFail,
			Message: fmt.Sprintf("0/%d %s pods ready", total, info.name),
			Cause:   "The ingress controller itself is down, so every Ingress of this class fails.",
			Try:     append(try, fmt.Sprintf("kubectl get events -n %s --sort-by=.lastTimestamp", firstNS)),
		})
	case ready < total:
		hop.Add(Check{
			Name:    "controller",
			Status:  StatusWarn,
			Message: fmt.Sprintf("%d/%d %s pods ready", ready, total, info.name),
			Cause:   "Some ingress controller replicas are not ready; traffic still flows with less capacity.",
			Try:     try,
		})
	default:
		hop.Add(Check{Name: "controller", Status: StatusOK, Message: fmt.Sprintf("%d/%d %s pods ready in %s", ready, total, info.name, joinKeys(namespaces))})
	}
	return nil
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func checkIngressAddress(ing *networkingv1.Ingress, hop *Hop) {
	var addrs []string
	for _, lb := range ing.Status.LoadBalancer.Ingress {
		if lb.IP != "" {
			addrs = append(addrs, lb.IP)
		} else if lb.Hostname != "" {
			addrs = append(addrs, lb.Hostname)
		}
	}
	if len(addrs) == 0 {
		hop.Add(Check{
			Name:    "address",
			Status:  StatusWarn,
			Message: "no address in status: no controller has admitted this Ingress (yet)",
			Cause:   "The controller either does not handle this Ingress's class or failed to program it. Cloud load balancers can also take a few minutes after creation.",
			Try:     []string{fmt.Sprintf("kubectl describe ingress %s -n %s", ing.Name, ing.Namespace)},
		})
		return
	}
	hop.Add(Check{Name: "address", Status: StatusOK, Message: strings.Join(addrs, ", ")})
}

// matchRule picks the rule and path that serve --host/--path the way
// controllers do, records the result as a check and returns the effective host
// and the backend (nil when nothing matches).
func (t *Tracer) matchRule(ing *networkingv1.Ingress, hop *Hop) (string, *networkingv1.IngressBackend) {
	host, reqPath := t.Options.Host, t.Options.Path
	if reqPath == "" {
		reqPath = "/"
	}

	if host == "" {
		var hosts []string
		seen := map[string]bool{}
		for _, r := range ing.Spec.Rules {
			if !seen[r.Host] {
				seen[r.Host] = true
				hosts = append(hosts, r.Host)
			}
		}
		if len(hosts) > 0 {
			host = hosts[0]
		}
		if len(hosts) > 1 {
			hop.Add(Check{
				Name:    "host",
				Status:  StatusInfo,
				Message: fmt.Sprintf("Ingress serves %d hosts; tracing %s (pick another with --host)", len(hosts), displayHost(host)),
			})
		}
	}

	// Controllers pick the most specific host first and only then match paths
	// within it; they do not fall through to a less specific host.
	best := 0
	var paths []networkingv1.HTTPIngressPath
	for _, r := range ing.Spec.Rules {
		tier := hostTier(r.Host, host)
		if tier == 0 || r.HTTP == nil {
			continue
		}
		if tier > best {
			best, paths = tier, nil
		}
		if tier == best {
			paths = append(paths, r.HTTP.Paths...)
		}
	}

	if p := matchPath(paths, reqPath); p != nil {
		hop.Add(Check{
			Name:    "rule",
			Status:  StatusOK,
			Message: fmt.Sprintf("%s%s matches path %s (%s) → %s", displayHost(host), reqPath, p.Path, pathType(p), describeBackend(&p.Backend)),
		})
		return host, &p.Backend
	}

	if db := ing.Spec.DefaultBackend; db != nil {
		hop.Add(Check{
			Name:    "rule",
			Status:  StatusInfo,
			Message: fmt.Sprintf("no rule matches %s%s; spec.defaultBackend → %s", displayHost(host), reqPath, describeBackend(db)),
		})
		return host, db
	}

	var details []string
	for _, r := range ing.Spec.Rules {
		if r.HTTP == nil {
			continue
		}
		for i := range r.HTTP.Paths {
			p := &r.HTTP.Paths[i]
			details = append(details, fmt.Sprintf("%s%s (%s) → %s", displayHost(r.Host), p.Path, pathType(p), describeBackend(&p.Backend)))
		}
	}
	if len(details) == 0 {
		details = []string{"the Ingress has no rules and no defaultBackend"}
	}
	hop.Add(Check{
		Name:    "rule",
		Status:  StatusFail,
		Message: fmt.Sprintf("no rule matches %s%s", displayHost(host), reqPath),
		Details: details,
		Cause:   "No rule matches this host and path, so the controller answers with its own default backend, typically a 404.",
		Try:     []string{fmt.Sprintf("kubectl get ingress %s -n %s -o yaml", ing.Name, ing.Namespace)},
	})
	return host, nil
}

// hostTier ranks how well a rule host matches a request host: 3 exact,
// 2 wildcard, 1 catch-all (empty rule host), 0 no match.
func hostTier(ruleHost, host string) int {
	switch {
	case ruleHost == host:
		return 3
	case wildcardMatch(ruleHost, host):
		return 2
	case ruleHost == "":
		return 1
	default:
		return 0
	}
}

// wildcardMatch implements Ingress wildcard hosts: "*.example.com" matches
// exactly one extra DNS label.
func wildcardMatch(pattern, host string) bool {
	suffix, ok := strings.CutPrefix(pattern, "*")
	if !ok || !strings.HasPrefix(suffix, ".") {
		return false
	}
	label, ok := strings.CutSuffix(host, suffix)
	return ok && label != "" && !strings.Contains(label, ".")
}

// matchPath returns the path a controller would choose for reqPath: the
// longest match, with Exact beating Prefix on a tie. ImplementationSpecific is
// treated as a plain string prefix, which is how ingress-nginx behaves without
// regex annotations.
func matchPath(paths []networkingv1.HTTPIngressPath, reqPath string) *networkingv1.HTTPIngressPath {
	var best *networkingv1.HTTPIngressPath
	bestScore := -1
	for i := range paths {
		p := &paths[i]
		path := p.Path
		if path == "" {
			path = "/"
		}
		score := -1
		switch pathType(p) {
		case networkingv1.PathTypeExact:
			if reqPath == path {
				score = 2*len(path) + 1
			}
		case networkingv1.PathTypePrefix:
			trimmed := strings.TrimSuffix(path, "/")
			if trimmed == "" || reqPath == trimmed || strings.HasPrefix(reqPath, trimmed+"/") {
				score = 2 * len(trimmed)
			}
		default:
			if strings.HasPrefix(reqPath, path) {
				score = 2 * len(path)
			}
		}
		if score > bestScore {
			best, bestScore = p, score
		}
	}
	return best
}

func pathType(p *networkingv1.HTTPIngressPath) networkingv1.PathType {
	if p.PathType == nil {
		return networkingv1.PathTypeImplementationSpecific
	}
	return *p.PathType
}

func displayHost(h string) string {
	if h == "" {
		return "*"
	}
	return h
}

func describeBackend(b *networkingv1.IngressBackend) string {
	switch {
	case b.Service != nil:
		return "svc/" + b.Service.Name + ":" + describeBackendPort(b.Service.Port)
	case b.Resource != nil:
		return b.Resource.Kind + "/" + b.Resource.Name
	default:
		return "(empty backend)"
	}
}

func describeBackendPort(p networkingv1.ServiceBackendPort) string {
	if p.Name != "" {
		return p.Name
	}
	return fmt.Sprint(p.Number)
}

// checkBackendPort verifies that the Service exposes the port the Ingress
// backend names. Ingress backends refer to the Service port, not targetPort.
func checkBackendPort(ing *networkingv1.Ingress, svc *corev1.Service, port networkingv1.ServiceBackendPort, hop *Hop) {
	want := describeBackendPort(port)
	var have []string
	for _, sp := range svc.Spec.Ports {
		if (port.Name != "" && sp.Name == port.Name) || (port.Name == "" && sp.Port == port.Number) {
			hop.Add(Check{Name: "backend", Status: StatusOK, Message: fmt.Sprintf("Service %s exposes port %s", svc.Name, want)})
			return
		}
		label := fmt.Sprint(sp.Port)
		if sp.Name != "" {
			label = sp.Name + "=" + label
		}
		have = append(have, label)
	}
	if len(have) == 0 {
		have = []string{"(none)"}
	}
	hop.Add(Check{
		Name:    "backend",
		Status:  StatusFail,
		Message: fmt.Sprintf("Service %s has no port %s", svc.Name, want),
		Details: []string{"service ports: " + strings.Join(have, ", ")},
		Cause:   "The Ingress backend names a port the Service does not expose (Ingress backends use the Service port, not the targetPort), so the controller has no upstream and answers 503.",
		Try: []string{
			fmt.Sprintf("kubectl get svc %s -n %s -o jsonpath='{.spec.ports}'", svc.Name, svc.Namespace),
			fmt.Sprintf("kubectl get ingress %s -n %s -o yaml", ing.Name, ing.Namespace),
		},
	})
}

// checkTLS validates the certificate the Ingress serves for host. Only the
// public certificate (tls.crt) is parsed; the private key is never inspected.
func (t *Tracer) checkTLS(ctx context.Context, ing *networkingv1.Ingress, host string, hop *Hop) error {
	if host == "" {
		hop.Add(Check{Name: "tls", Status: StatusInfo, Message: "no host to check the certificate against; pass --host"})
		return nil
	}

	// An entry that lists the host wins over one without hosts.
	var entry, catchAll *networkingv1.IngressTLS
	for i := range ing.Spec.TLS {
		tls := &ing.Spec.TLS[i]
		if len(tls.Hosts) == 0 && catchAll == nil {
			catchAll = tls
		}
		for _, h := range tls.Hosts {
			if entry == nil && (h == host || wildcardMatch(h, host)) {
				entry = tls
			}
		}
	}
	if entry == nil {
		entry = catchAll
	}
	if entry == nil {
		hop.Add(Check{
			Name:    "tls",
			Status:  StatusWarn,
			Message: fmt.Sprintf("no TLS entry covers %s", host),
			Cause:   "HTTPS requests for this host get the controller's default certificate, which clients usually reject.",
			Try:     []string{fmt.Sprintf("kubectl get ingress %s -n %s -o jsonpath='{.spec.tls}'", ing.Name, ing.Namespace)},
		})
		return nil
	}
	if entry.SecretName == "" {
		hop.Add(Check{Name: "tls", Status: StatusInfo, Message: fmt.Sprintf("TLS entry for %s has no secretName; the controller's default certificate is used", host)})
		return nil
	}

	ns := ing.Namespace
	trySecret := fmt.Sprintf("kubectl get secret %s -n %s", entry.SecretName, ns)
	secret, err := t.Client.CoreV1().Secrets(ns).Get(ctx, entry.SecretName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		hop.Add(Check{
			Name:    "tls",
			Status:  StatusFail,
			Message: fmt.Sprintf("TLS secret %q not found", entry.SecretName),
			Cause:   "The controller falls back to its default, usually self-signed, certificate, so HTTPS clients reject the connection. If cert-manager issues this secret, check the Certificate resource.",
			Try:     []string{trySecret, fmt.Sprintf("kubectl get certificate -n %s", ns)},
		})
		return nil
	case apierrors.IsForbidden(err):
		hop.Add(Check{Name: "tls", Status: StatusInfo, Message: fmt.Sprintf("not allowed to read secret %s; certificate not checked", entry.SecretName)})
		return nil
	case err != nil:
		return fmt.Errorf("get secret %s/%s: %w", ns, entry.SecretName, err)
	}

	cert, err := parseCert(secret.Data[corev1.TLSCertKey])
	if err != nil {
		hop.Add(Check{
			Name:    "tls",
			Status:  StatusFail,
			Message: fmt.Sprintf("secret %s: %v", entry.SecretName, err),
			Cause:   "The secret does not hold a usable certificate, so the controller cannot serve it.",
			Try:     []string{trySecret + " -o jsonpath='{.type}'"},
		})
		return nil
	}

	now := t.now()
	sans := strings.Join(cert.DNSNames, ", ")
	if sans == "" {
		sans = "(none)"
	}
	tryCert := fmt.Sprintf("kubectl get secret %s -n %s -o jsonpath='{.data.tls\\.crt}' | base64 -d | openssl x509 -noout -subject -dates -ext subjectAltName", entry.SecretName, ns)
	fail := func(msg, cause string) {
		hop.Add(Check{Name: "tls", Status: StatusFail, Message: msg, Details: []string{"SANs: " + sans}, Cause: cause, Try: []string{tryCert}})
	}
	switch {
	case now.After(cert.NotAfter):
		fail(fmt.Sprintf("certificate expired on %s", cert.NotAfter.UTC().Format(time.DateOnly)),
			"The certificate has expired, so HTTPS clients refuse the connection. Renew it (or check why cert-manager did not).")
	case now.Before(cert.NotBefore):
		fail(fmt.Sprintf("certificate is not valid until %s", cert.NotBefore.UTC().Format(time.DateOnly)),
			"The certificate is not valid yet; check the issuer and the node clocks.")
	case cert.VerifyHostname(host) != nil:
		fail(fmt.Sprintf("certificate does not cover %s", host),
			"The certificate's subject alternative names do not include this host, so clients report a name mismatch.")
	case cert.NotAfter.Sub(now) < certExpiryWarning:
		hop.Add(Check{
			Name:    "tls",
			Status:  StatusWarn,
			Message: fmt.Sprintf("certificate for %s expires in %d day(s)", host, daysUntil(now, cert.NotAfter)),
			Cause:   "The certificate is about to expire and does not look renewed yet.",
			Try:     []string{tryCert},
		})
	default:
		hop.Add(Check{Name: "tls", Status: StatusOK, Message: fmt.Sprintf("certificate for %s valid until %s (%d days)", host, cert.NotAfter.UTC().Format(time.DateOnly), daysUntil(now, cert.NotAfter))})
	}
	return nil
}

func parseCert(data []byte) (*x509.Certificate, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%s is empty", corev1.TLSCertKey)
	}
	for rest := data; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, fmt.Errorf("%s holds no PEM certificate", corev1.TLSCertKey)
		}
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", corev1.TLSCertKey, err)
			}
			return cert, nil
		}
	}
}

func daysUntil(now, t time.Time) int {
	return int(t.Sub(now).Hours() / 24)
}
