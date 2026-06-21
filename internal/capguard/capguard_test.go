package capguard

import (
	"strings"
	"testing"
)

func TestEvaluate_RuleOfTwo(t *testing.T) {
	tests := []struct {
		name    string
		caps    Capabilities
		gate    bool
		want    Verdict
		wantAxN int
	}{
		{
			name: "two axes (untrusted + private) is allowed",
			caps: Capabilities{UntrustedInput: true, PrivateData: true},
			gate: false, want: Allow, wantAxN: 2,
		},
		{
			name: "two axes (untrusted + open egress) is allowed",
			caps: Capabilities{UntrustedInput: true, OpenEgress: true},
			gate: false, want: Allow, wantAxN: 2,
		},
		{
			name: "trifecta with a gate available requires the gate",
			caps: Capabilities{UntrustedInput: true, PrivateData: true, OpenEgress: true},
			gate: true, want: GateRequired, wantAxN: 3,
		},
		{
			name: "trifecta with no gate refuses (fail closed)",
			caps: Capabilities{UntrustedInput: true, PrivateData: true, OpenEgress: true},
			gate: false, want: Refuse, wantAxN: 3,
		},
		{
			name: "scoped allowlist is not open egress",
			caps: Capabilities{UntrustedInput: true, PrivateData: true, EgressHosts: []string{"a.test", "b.test"}},
			gate: false, want: Allow, wantAxN: 2,
		},
		{
			name: "wildcard host counts as open egress → trifecta",
			caps: Capabilities{UntrustedInput: true, PrivateData: true, EgressHosts: []string{"*.com"}},
			gate: true, want: GateRequired, wantAxN: 3,
		},
		{
			name:    "no axes active is trivially allowed",
			caps:    Capabilities{},
			gate:    false,
			want:    Allow,
			wantAxN: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Evaluate(tc.caps, tc.gate)
			if d.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q (detail: %s)", d.Verdict, tc.want, d.Detail)
			}
			if len(d.Axes) != tc.wantAxN {
				t.Fatalf("active axes = %v (n=%d), want n=%d", d.Axes, len(d.Axes), tc.wantAxN)
			}
			if d.Detail == "" {
				t.Fatal("decision detail must never be empty (it feeds the audit log / gate prompt)")
			}
		})
	}
}

func TestOpenEgressThreshold(t *testing.T) {
	// Exactly at the threshold is still scoped; one over is open.
	hosts := make([]string, OpenEgressThreshold)
	for i := range hosts {
		hosts[i] = "h"
	}
	c := Capabilities{UntrustedInput: true, PrivateData: true, EgressHosts: hosts}
	if c.Trifecta() {
		t.Fatalf("%d hosts should not count as open egress", OpenEgressThreshold)
	}
	c.EgressHosts = append(c.EgressHosts, "one-more")
	if !c.Trifecta() {
		t.Fatalf("%d hosts should count as open egress", OpenEgressThreshold+1)
	}
}

func TestDescribeUsesReasons(t *testing.T) {
	c := Capabilities{
		UntrustedInput: true,
		PrivateData:    true,
		OpenEgress:     true,
		Reasons:        map[string]string{"A": "browse-agent", "B": "repo-mounted", "C": "profile:web-research"},
	}
	d := Evaluate(c, true)
	for _, want := range []string{"browse-agent", "repo-mounted", "profile:web-research"} {
		if !strings.Contains(d.Detail, want) {
			t.Errorf("detail %q missing reason %q", d.Detail, want)
		}
	}
}
