package trace

import (
	"encoding/json"
	"testing"
)

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in      string
		want    Target
		wantErr bool
	}{
		{in: "svc/web", want: Target{Kind: KindService, Namespace: "ns", Name: "web"}},
		{in: "Service/web", want: Target{Kind: KindService, Namespace: "ns", Name: "web"}},
		{in: "ing/shop", want: Target{Kind: KindIngress, Namespace: "ns", Name: "shop"}},
		{in: "deployments/api", want: Target{Kind: KindDeployment, Namespace: "ns", Name: "api"}},
		{in: "web", wantErr: true},
		{in: "svc/", wantErr: true},
		{in: "/web", wantErr: true},
		{in: "svc/a/b", wantErr: true},
		{in: "pod/web-1", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseTarget(tc.in, "ns")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestReportAggregation(t *testing.T) {
	hop := func(statuses ...Status) Hop {
		h := Hop{Kind: "X", Name: "x"}
		for _, s := range statuses {
			h.Add(Check{Name: s.String(), Status: s})
		}
		return h
	}
	tests := []struct {
		name       string
		hops       []Hop
		wantStatus Status
		wantBroken int
		wantExit   int
		wantIssue  string
	}{
		{"all ok", []Hop{hop(StatusOK, StatusInfo), hop(StatusOK)}, StatusOK, -1, ExitOK, ""},
		{"info and skip do not escalate", []Hop{hop(StatusInfo, StatusSkip)}, StatusOK, -1, ExitOK, ""},
		{"warning", []Hop{hop(StatusOK), hop(StatusWarn)}, StatusWarn, -1, ExitWarning, "warn"},
		{"fail wins over warn", []Hop{hop(StatusWarn), hop(StatusOK, StatusFail), hop(StatusFail)}, StatusFail, 1, ExitBroken, "fail"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Report{Hops: tc.hops}
			r.Finalize()
			if r.Status != tc.wantStatus || r.FirstBroken != tc.wantBroken || r.ExitCode() != tc.wantExit {
				t.Errorf("got status=%s broken=%d exit=%d, want %s %d %d",
					r.Status, r.FirstBroken, r.ExitCode(), tc.wantStatus, tc.wantBroken, tc.wantExit)
			}
			issue := r.PrimaryIssue()
			switch {
			case tc.wantIssue == "" && issue != nil:
				t.Errorf("unexpected primary issue %+v", issue)
			case tc.wantIssue != "" && (issue == nil || issue.Name != tc.wantIssue):
				t.Errorf("primary issue = %+v, want %q", issue, tc.wantIssue)
			}
		})
	}
}

func TestStatusJSONRoundTrip(t *testing.T) {
	for s := range statusNames {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var got Status
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got != s {
			t.Errorf("round trip %s → %s → %s", s, b, got)
		}
	}
	var s Status
	if err := json.Unmarshal([]byte(`"bogus"`), &s); err == nil {
		t.Error("unmarshal of unknown status succeeded")
	}
}
