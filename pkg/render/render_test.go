package render

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rasmabayu/kubectl-hops/pkg/trace"
)

func brokenReport() *trace.Report {
	svc := trace.Hop{Kind: "Service", Namespace: "shop", Name: "web"}
	svc.Add(trace.Check{Name: "exists", Status: trace.StatusOK, Message: "ClusterIP"})
	svc.Add(trace.Check{
		Name: "selector", Status: trace.StatusFail, Message: "selector app=web matches 0 pods",
		Details: []string{"app=web matches 0 pod(s)"},
		Cause:   "labels do not match",
		Try:     []string{"kubectl get pods -n shop --show-labels"},
	})
	eps := trace.Hop{Kind: "EndpointSlices", Namespace: "shop", Name: "web"}
	eps.Add(trace.Check{Name: "endpoints", Status: trace.StatusFail, Message: "0 endpoints"})
	r := &trace.Report{Target: "Service shop/web", Hops: []trace.Hop{svc, eps}}
	r.Finalize()
	return r
}

func TestText(t *testing.T) {
	healthy := trace.Hop{Kind: "Service", Namespace: "shop", Name: "web"}
	healthy.Add(trace.Check{Name: "exists", Status: trace.StatusOK, Message: "ClusterIP"})
	okReport := &trace.Report{Target: "Service shop/web", Hops: []trace.Hop{healthy}}
	okReport.Finalize()

	warnHop := trace.Hop{Kind: "EndpointSlices", Namespace: "shop", Name: "web"}
	warnHop.Add(trace.Check{Name: "endpoints", Status: trace.StatusWarn, Message: "1/2 endpoints ready", Cause: "some not ready"})
	warnReport := &trace.Report{Target: "Service shop/web", Hops: []trace.Hop{healthy, warnHop}}
	warnReport.Finalize()

	tests := []struct {
		name    string
		report  *trace.Report
		want    []string
		notWant []string
	}{
		{
			name:   "broken",
			report: brokenReport(),
			want: []string{
				"Tracing Service shop/web",
				"✖ Service shop/web  ← first broken hop",
				"├─ ✔ exists: ClusterIP",
				"└─ ✖ selector: selector app=web matches 0 pods",
				"      app=web matches 0 pod(s)",
				"  ↓\n✖ EndpointSlices shop/web\n",
				"Result: BROKEN at Service shop/web",
				"Likely cause: labels do not match",
				"Try:\n  kubectl get pods -n shop --show-labels",
			},
			notWant: []string{"EndpointSlices shop/web  ← first broken hop", "\x1b["},
		},
		{
			name:    "degraded",
			report:  warnReport,
			want:    []string{"⚠ EndpointSlices shop/web", "Result: DEGRADED", "Likely cause: some not ready"},
			notWant: []string{"first broken hop", "Try:"},
		},
		{
			name:    "ok",
			report:  okReport,
			want:    []string{"✔ Service shop/web", "└─ ✔ exists: ClusterIP", "Result: OK"},
			notWant: []string{"Likely cause", "↓"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Text(&buf, tc.report, Options{}); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q\n---\n%s", w, out)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(out, nw) {
					t.Errorf("output unexpectedly contains %q\n---\n%s", nw, out)
				}
			}
		})
	}
}

func TestTextColor(t *testing.T) {
	var buf bytes.Buffer
	if err := Text(&buf, brokenReport(), Options{Color: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), ansiRed+"✖"+ansiReset) {
		t.Errorf("colored output has no red ✖:\n%q", buf.String())
	}
}

func TestJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, brokenReport(), FormatJSON, Options{Color: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "\x1b[") {
		t.Error("JSON output contains ANSI codes")
	}
	var got struct {
		Status         string `json:"status"`
		FirstBrokenHop int    `json:"firstBrokenHop"`
		Hops           []struct {
			Kind   string `json:"kind"`
			Status string `json:"status"`
			Checks []struct {
				Name        string   `json:"name"`
				Status      string   `json:"status"`
				LikelyCause string   `json:"likelyCause"`
				Try         []string `json:"try"`
			} `json:"checks"`
		} `json:"hops"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if got.Status != "fail" || got.FirstBrokenHop != 0 || len(got.Hops) != 2 {
		t.Fatalf("unexpected top level: %+v", got)
	}
	sel := got.Hops[0].Checks[1]
	if sel.Name != "selector" || sel.Status != "fail" || sel.LikelyCause == "" || len(sel.Try) != 1 {
		t.Errorf("unexpected selector check: %+v", sel)
	}
}

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]Format{"text": FormatText, "JSON": FormatJSON} {
		if got, err := ParseFormat(in); err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseFormat("yaml"); err == nil {
		t.Error("ParseFormat(yaml) succeeded")
	}
}
