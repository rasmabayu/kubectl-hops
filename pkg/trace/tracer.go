package trace

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"
)

// Options tune a trace. Host and Path select an Ingress rule.
type Options struct {
	Host string
	Path string
}

// Tracer runs read-only checks against a cluster.
type Tracer struct {
	Client  kubernetes.Interface
	Options Options
}

// Trace walks the traffic path that starts at target.
func (t *Tracer) Trace(ctx context.Context, target Target) (*Report, error) {
	r := &Report{Target: target.String()}
	switch target.Kind {
	case KindService:
		hops, err := t.traceService(ctx, target.Namespace, target.Name)
		if err != nil {
			return nil, err
		}
		r.Hops = hops
	case KindIngress, KindDeployment:
		return nil, fmt.Errorf("tracing from %s is not implemented yet; start from svc/<name> for now", target.Kind)
	default:
		return nil, fmt.Errorf("unsupported kind %q", target.Kind)
	}
	r.Finalize()
	return r, nil
}
