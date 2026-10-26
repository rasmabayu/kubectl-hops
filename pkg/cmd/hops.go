// Package cmd wires the kubectl-hops command line.
package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"

	"github.com/rasmabayu/kubectl-hops/pkg/render"
	"github.com/rasmabayu/kubectl-hops/pkg/trace"
)

const example = `  # Why does my Service not answer?
  kubectl hops svc/web -n shop

  # Follow the Ingress rule for a host and path, including its TLS certificate
  kubectl hops ingress/shop -n shop --host shop.example.com --path /api --check-tls

  # Find the Services in front of a Deployment and explain its unhealthy pods
  kubectl hops deploy/api -n shop

  # Machine-readable output for CI; exit code 1 = broken, 2 = warnings
  kubectl hops svc/web -n shop -o json`

// Version is set at build time via -ldflags.
var Version = "dev"

// Options holds everything the command needs to run.
type Options struct {
	ConfigFlags *genericclioptions.ConfigFlags
	IOStreams   genericiooptions.IOStreams

	Host     string
	Path     string
	CheckTLS bool
	Output   string
	Color    string

	// newClient is swapped out in tests.
	newClient func() (kubernetes.Interface, error)
}

// exitError carries a process exit code out of cobra.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// ExitCode extracts the exit code that Execute should terminate with.
func ExitCode(err error) int {
	if err == nil {
		return trace.ExitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return trace.ExitError
}

// NewCmdHops builds the root command.
func NewCmdHops(streams genericiooptions.IOStreams) *cobra.Command {
	return newCmd(NewOptions(streams))
}

// NewOptions returns Options that talk to the cluster from the kubeconfig flags.
func NewOptions(streams genericiooptions.IOStreams) *Options {
	o := &Options{
		ConfigFlags: genericclioptions.NewConfigFlags(true),
		IOStreams:   streams,
	}
	o.newClient = func() (kubernetes.Interface, error) {
		cfg, err := o.ConfigFlags.ToRESTConfig()
		if err != nil {
			return nil, err
		}
		return kubernetes.NewForConfig(cfg)
	}
	return o
}

func newCmd(o *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use: "hops (ingress|svc|deploy)/NAME [flags]",
		// Shown as "kubectl hops" in help and version output.
		Annotations: map[string]string{cobra.CommandDisplayNameAnnotation: "kubectl hops"},
		Short:       "Trace the traffic path to a workload and explain where it breaks",
		Long:        "kubectl hops follows Ingress → Service → EndpointSlices → Pods, checks each hop with read-only API calls and points at the first one that breaks, with a likely cause and commands to try.",
		Example:     example,
		Version:     Version,
		Args:        cobra.ExactArgs(1),
		// Usage on every broken trace would drown the report.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			return o.Run(c, args[0])
		},
	}
	cmd.Flags().StringVar(&o.Host, "host", "", "Ingress host to match; a full URL also sets --path (ingress targets)")
	cmd.Flags().StringVar(&o.Path, "path", "/", "Ingress path to match (ingress targets)")
	cmd.Flags().StringVarP(&o.Output, "output", "o", "text", "Output format: text or json")
	cmd.Flags().StringVar(&o.Color, "color", "auto", "Colorize text output: auto, always or never")
	cmd.Flags().BoolVar(&o.CheckTLS, "check-tls", false, "Also check the Ingress TLS certificate (needs get on secrets)")
	// Defined by hand so cobra does not claim -v, which kubectl uses for log verbosity.
	cmd.Flags().Bool("version", false, "Print the version and exit")
	o.ConfigFlags.AddFlags(cmd.Flags())
	return cmd
}

// Run executes one trace and maps its verdict to an exit code.
func (o *Options) Run(c *cobra.Command, arg string) error {
	format, err := render.ParseFormat(o.Output)
	if err != nil {
		return err
	}
	color, err := o.useColor()
	if err != nil {
		return err
	}
	ns, _, err := o.ConfigFlags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	target, err := trace.ParseTarget(arg, ns)
	if err != nil {
		return err
	}
	host, urlPath, err := trace.ParseHost(o.Host)
	if err != nil {
		return err
	}
	path := o.Path
	// "--host https://shop.example.com/api" also sets the path, unless --path says otherwise.
	if urlPath != "" && !c.Flags().Changed("path") {
		path = urlPath
	}
	client, err := o.newClient()
	if err != nil {
		return err
	}

	t := &trace.Tracer{Client: client, Options: trace.Options{Host: host, Path: path, CheckTLS: o.CheckTLS}}
	report, err := t.Trace(c.Context(), target)
	if err != nil {
		return err
	}
	if err := render.Write(o.IOStreams.Out, report, format, render.Options{Color: color}); err != nil {
		return err
	}
	if code := report.ExitCode(); code != trace.ExitOK {
		return &exitError{code: code}
	}
	return nil
}

func (o *Options) useColor() (bool, error) {
	switch o.Color {
	case "always":
		return true, nil
	case "never":
		return false, nil
	case "auto":
		if _, set := os.LookupEnv("NO_COLOR"); set {
			return false, nil
		}
		f, ok := o.IOStreams.Out.(*os.File)
		return ok && term.IsTerminal(int(f.Fd())), nil
	default:
		return false, fmt.Errorf("unknown --color value %q: use auto, always or never", o.Color)
	}
}
