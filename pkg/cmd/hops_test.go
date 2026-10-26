package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/rasmabayu/kubectl-hops/pkg/trace"
)

// run executes the command against a fake cluster and returns stdout and the
// exit code main would use.
func run(t *testing.T, objects []runtime.Object, args ...string) (string, int, error) {
	t.Helper()
	// Keep the developer's real kubeconfig out of the test.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "none"))

	streams, _, out, _ := genericiooptions.NewTestIOStreams()
	o := NewOptions(streams)
	o.newClient = func() (kubernetes.Interface, error) { return fake.NewClientset(objects...), nil }
	c := newCmd(o)
	c.SetArgs(args)
	c.SetOut(out)
	c.SetErr(out)
	err := c.ExecuteContext(context.Background())
	return out.String(), ExitCode(err), err
}

func TestRunExitCodes(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80}},
		},
	}
	ext := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "db.example.com"},
	}

	tests := []struct {
		name     string
		args     []string
		wantExit int
		wantOut  string
	}{
		{"ok", []string{"svc/db", "-n", "shop"}, trace.ExitOK, "Result: OK"},
		{"broken", []string{"svc/web", "-n", "shop"}, trace.ExitBroken, "Result: BROKEN at Service shop/web"},
		{"missing", []string{"svc/nope", "-n", "shop"}, trace.ExitBroken, `"nope" not found`},
		{"json", []string{"svc/web", "-n", "shop", "-o", "json"}, trace.ExitBroken, `"firstBrokenHop": 0`},
		{"bad target", []string{"web", "-n", "shop"}, trace.ExitError, ""},
		{"bad output", []string{"svc/web", "-o", "yaml"}, trace.ExitError, ""},
		{"bad color", []string{"svc/web", "--color", "rainbow"}, trace.ExitError, ""},
		{"ingress missing", []string{"ingress/shop", "-n", "shop", "--host", "a.example.com", "--check-tls"}, trace.ExitBroken, `Ingress "shop" not found`},
		{"not implemented", []string{"deploy/web", "-n", "shop"}, trace.ExitError, ""},
		{"version", []string{"--version"}, trace.ExitOK, "kubectl hops version"},
		{"-v is not version", []string{"-v"}, trace.ExitError, ""},
		{"no args", nil, trace.ExitError, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, code, err := run(t, []runtime.Object{svc, ext}, tc.args...)
			if code != tc.wantExit {
				t.Fatalf("exit = %d (err %v), want %d\n%s", code, err, tc.wantExit, out)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("output missing %q\n%s", tc.wantOut, out)
			}
			if strings.Contains(out, "\x1b[") {
				t.Errorf("non-terminal output is colored")
			}
		})
	}
}

func TestRunJSONIsValid(t *testing.T) {
	out, _, _ := run(t, nil, "svc/web", "-n", "shop", "-o", "json")
	var r trace.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if r.Status != trace.StatusFail {
		t.Errorf("status = %s, want fail", r.Status)
	}
}

func TestColorAlways(t *testing.T) {
	out, _, _ := run(t, nil, "svc/web", "-n", "shop", "--color", "always")
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("--color always produced no ANSI codes")
	}
}

func TestExitCode(t *testing.T) {
	if ExitCode(nil) != trace.ExitOK {
		t.Error("nil error should exit 0")
	}
	if ExitCode(&exitError{code: 2}) != 2 {
		t.Error("exitError code not propagated")
	}
	if ExitCode(bytes.ErrTooLarge) != trace.ExitError {
		t.Error("generic error should exit 3")
	}
}
