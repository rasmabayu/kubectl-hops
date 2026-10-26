// Package trace walks the traffic path of a Kubernetes workload hop by hop
// (Ingress → Service → EndpointSlices → Pods) and records what it finds.
// It only ever reads from the API server.
package trace

import (
	"encoding/json"
	"fmt"
)

// Status is the outcome of a single check or of a whole hop.
type Status int

const (
	// StatusOK means the check passed.
	StatusOK Status = iota
	// StatusInfo is a neutral note that never affects the exit code.
	StatusInfo
	// StatusWarn means traffic may work but something looks off.
	StatusWarn
	// StatusFail means traffic cannot flow past this point.
	StatusFail
	// StatusSkip means the check was not run, usually because an earlier one failed.
	StatusSkip
)

var statusNames = map[Status]string{
	StatusOK:   "ok",
	StatusInfo: "info",
	StatusWarn: "warn",
	StatusFail: "fail",
	StatusSkip: "skip",
}

func (s Status) String() string {
	if n, ok := statusNames[s]; ok {
		return n
	}
	return fmt.Sprintf("Status(%d)", int(s))
}

// MarshalJSON renders the status as its lowercase name.
func (s Status) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// UnmarshalJSON parses a lowercase status name.
func (s *Status) UnmarshalJSON(b []byte) error {
	var name string
	if err := json.Unmarshal(b, &name); err != nil {
		return err
	}
	for k, v := range statusNames {
		if v == name {
			*s = k
			return nil
		}
	}
	return fmt.Errorf("unknown status %q", name)
}

// severity orders statuses for aggregation; Info and Skip never escalate a hop.
func (s Status) severity() int {
	switch s {
	case StatusFail:
		return 3
	case StatusWarn:
		return 2
	default:
		return 0
	}
}

// Check is one assertion made about a hop.
type Check struct {
	Name    string   `json:"name"`
	Status  Status   `json:"status"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
	// Cause is a plain-language explanation of the most likely reason for a
	// warning or failure. Empty for passing checks.
	Cause string `json:"likelyCause,omitempty"`
	// Try lists concrete, read-only commands that help confirm the cause.
	Try []string `json:"try,omitempty"`
}

// Hop is one object on the traffic path.
type Hop struct {
	Kind      string  `json:"kind"`
	Namespace string  `json:"namespace,omitempty"`
	Name      string  `json:"name"`
	Status    Status  `json:"status"`
	Checks    []Check `json:"checks"`
}

// Add appends a check and folds its status into the hop's status.
func (h *Hop) Add(c Check) {
	h.Checks = append(h.Checks, c)
	if c.Status.severity() > h.Status.severity() {
		h.Status = c.Status
	}
}

// Ref is a short "kind namespace/name" label for the hop.
func (h *Hop) Ref() string {
	if h.Namespace == "" {
		return h.Kind + " " + h.Name
	}
	return h.Kind + " " + h.Namespace + "/" + h.Name
}

// Report is the full result of tracing one target.
type Report struct {
	Target string `json:"target"`
	Status Status `json:"status"`
	Hops   []Hop  `json:"hops"`
	// FirstBroken is the index into Hops of the first failing hop, or -1.
	FirstBroken int `json:"firstBrokenHop"`
}

// Finalize computes the overall status and the first broken hop. It must be
// called after all hops have been added.
func (r *Report) Finalize() {
	r.Status = StatusOK
	r.FirstBroken = -1
	for i, h := range r.Hops {
		if h.Status == StatusFail && r.FirstBroken < 0 {
			r.FirstBroken = i
		}
		if h.Status.severity() > r.Status.severity() {
			r.Status = h.Status
		}
	}
}

// PrimaryIssue returns the check that best explains the report's status: the
// first failing check of the first broken hop, otherwise the first warning.
// Checks that carry a likely cause are preferred over bare summaries.
func (r *Report) PrimaryIssue() *Check {
	if r.FirstBroken >= 0 {
		if c := firstWith(r.Hops[r.FirstBroken:r.FirstBroken+1], StatusFail); c != nil {
			return c
		}
	}
	return firstWith(r.Hops, StatusWarn)
}

func firstWith(hops []Hop, s Status) *Check {
	var bare *Check
	for hi := range hops {
		for ci := range hops[hi].Checks {
			c := &hops[hi].Checks[ci]
			if c.Status != s {
				continue
			}
			if c.Cause != "" {
				return c
			}
			if bare == nil {
				bare = c
			}
		}
	}
	return bare
}

// Exit codes, chosen so the tool can gate CI pipelines.
const (
	ExitOK      = 0
	ExitBroken  = 1
	ExitWarning = 2
	// ExitError means the trace itself could not run (bad args, API errors).
	ExitError = 3
)

// ExitCode maps the overall status to a process exit code.
func (r *Report) ExitCode() int {
	switch r.Status {
	case StatusFail:
		return ExitBroken
	case StatusWarn:
		return ExitWarning
	default:
		return ExitOK
	}
}
