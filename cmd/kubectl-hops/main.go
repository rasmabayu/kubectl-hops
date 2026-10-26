// Command kubectl-hops is a kubectl plugin that traces the traffic path to a
// workload and explains where it breaks.
package main

import (
	"fmt"
	"os"

	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/rasmabayu/kubectl-hops/pkg/cmd"
	"github.com/rasmabayu/kubectl-hops/pkg/trace"
)

func main() {
	streams := genericiooptions.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}
	err := cmd.NewCmdHops(streams).Execute()
	code := cmd.ExitCode(err)
	if code == trace.ExitError {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	os.Exit(code)
}
