package trace

import (
	"fmt"
	"strings"
)

// Kind is the type of object a trace starts from.
type Kind string

const (
	KindIngress    Kind = "Ingress"
	KindService    Kind = "Service"
	KindDeployment Kind = "Deployment"
)

var kindAliases = map[string]Kind{
	"ing":         KindIngress,
	"ingress":     KindIngress,
	"ingresses":   KindIngress,
	"svc":         KindService,
	"service":     KindService,
	"services":    KindService,
	"deploy":      KindDeployment,
	"deployment":  KindDeployment,
	"deployments": KindDeployment,
}

// Target identifies where a trace starts.
type Target struct {
	Kind      Kind
	Namespace string
	Name      string
}

func (t Target) String() string {
	return fmt.Sprintf("%s %s/%s", t.Kind, t.Namespace, t.Name)
}

// ParseTarget parses "kind/name" (e.g. svc/web, ingress/shop, deploy/api).
func ParseTarget(s, namespace string) (Target, error) {
	kind, name, ok := strings.Cut(s, "/")
	if !ok || kind == "" || name == "" || strings.Contains(name, "/") {
		return Target{}, fmt.Errorf("target %q must look like kind/name, e.g. svc/web", s)
	}
	k, ok := kindAliases[strings.ToLower(kind)]
	if !ok {
		return Target{}, fmt.Errorf("unsupported kind %q: use ingress, svc or deploy", kind)
	}
	return Target{Kind: k, Namespace: namespace, Name: name}, nil
}
