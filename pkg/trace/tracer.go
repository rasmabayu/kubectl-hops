package trace

import (
	"context"
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
)

// Options tune a trace. Host and Path select an Ingress rule.
type Options struct {
	Host string
	Path string
	// CheckTLS reads the Ingress TLS secret and validates its certificate.
	CheckTLS bool
}

// Tracer runs read-only checks against a cluster.
type Tracer struct {
	Client  kubernetes.Interface
	Options Options
	// Now is the clock for certificate checks; nil means time.Now.
	Now func() time.Time
}

func (t *Tracer) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Trace walks the traffic path that starts at target.
func (t *Tracer) Trace(ctx context.Context, target Target) (*Report, error) {
	r := &Report{Target: target.String()}
	var (
		hops []Hop
		err  error
	)
	switch target.Kind {
	case KindIngress:
		hops, err = t.traceIngress(ctx, target.Namespace, target.Name)
	case KindService:
		hops, err = t.traceService(ctx, target.Namespace, target.Name)
	case KindDeployment:
		hops, err = t.traceDeployment(ctx, target.Namespace, target.Name)
	default:
		return nil, fmt.Errorf("unsupported kind %q", target.Kind)
	}
	if err != nil {
		return nil, err
	}
	r.Hops = hops
	r.Finalize()
	return r, nil
}
