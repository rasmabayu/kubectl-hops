// Package render turns a trace.Report into human or machine readable output.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/rasmabayu/kubectl-hops/pkg/trace"
)

// Format is an output format accepted by -o.
type Format string

const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

// ParseFormat validates a -o value.
func ParseFormat(s string) (Format, error) {
	switch f := Format(strings.ToLower(s)); f {
	case FormatText, FormatJSON:
		return f, nil
	default:
		return "", fmt.Errorf("unknown output format %q: use text or json", s)
	}
}

// Options control text rendering.
type Options struct {
	Color bool
}

// Write renders r in the given format.
func Write(w io.Writer, r *trace.Report, f Format, o Options) error {
	if f == FormatJSON {
		return JSON(w, r)
	}
	return Text(w, r, o)
}

// JSON writes r as indented JSON.
func JSON(w io.Writer, r *trace.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

type painter struct{ on bool }

func (p painter) paint(code, s string) string {
	if !p.on {
		return s
	}
	return code + s + ansiReset
}

func (p painter) status(s trace.Status) string {
	switch s {
	case trace.StatusOK:
		return p.paint(ansiGreen, "✔")
	case trace.StatusWarn:
		return p.paint(ansiYellow, "⚠")
	case trace.StatusFail:
		return p.paint(ansiRed, "✖")
	case trace.StatusInfo:
		return p.paint(ansiCyan, "ℹ")
	default:
		return p.paint(ansiDim, "-")
	}
}

// Text writes r as a per-hop tree followed by a verdict, the likely cause and
// commands to try.
func Text(w io.Writer, r *trace.Report, o Options) error {
	p := painter{on: o.Color}
	var b strings.Builder

	fmt.Fprintf(&b, "Tracing %s\n\n", r.Target)
	for i, h := range r.Hops {
		if i > 0 {
			b.WriteString(p.paint(ansiDim, "  ↓") + "\n")
		}
		title := p.paint(ansiBold, h.Ref())
		if i == r.FirstBroken {
			title = p.paint(ansiBold+ansiRed, h.Ref()) + "  " + p.paint(ansiRed, "← first broken hop")
		}
		fmt.Fprintf(&b, "%s %s\n", p.status(h.Status), title)
		for ci, c := range h.Checks {
			branch, cont := "├─", "│ "
			if ci == len(h.Checks)-1 {
				branch, cont = "└─", "  "
			}
			fmt.Fprintf(&b, "%s %s %s: %s\n", p.paint(ansiDim, branch), p.status(c.Status), c.Name, c.Message)
			for _, d := range c.Details {
				fmt.Fprintf(&b, "%s    %s\n", p.paint(ansiDim, cont), p.paint(ansiDim, d))
			}
		}
	}

	b.WriteString("\n")
	switch r.Status {
	case trace.StatusFail:
		fmt.Fprintf(&b, "Result: %s at %s\n", p.paint(ansiBold+ansiRed, "BROKEN"), r.Hops[r.FirstBroken].Ref())
	case trace.StatusWarn:
		fmt.Fprintf(&b, "Result: %s (traffic flows, but check the warnings)\n", p.paint(ansiBold+ansiYellow, "DEGRADED"))
	default:
		fmt.Fprintf(&b, "Result: %s\n", p.paint(ansiBold+ansiGreen, "OK"))
	}
	if c := r.PrimaryIssue(); c != nil {
		if c.Cause != "" {
			fmt.Fprintf(&b, "Likely cause: %s\n", c.Cause)
		}
		if len(c.Try) > 0 {
			b.WriteString("Try:\n")
			for _, t := range c.Try {
				fmt.Fprintf(&b, "  %s\n", t)
			}
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}
