package capability_test

import (
	"strings"
	"testing"

	"nilcore/internal/capability"
	"nilcore/internal/capguard"
	"nilcore/internal/policy"
)

func TestForModeMatchesLegacyCapabilityChoice(t *testing.T) {
	// The byte-identical contract with cmd/nilcore's capabilityForMode:
	// read-only modes ⇒ write-free + ReadOnlyCommandPolicy + shell off; full
	// modes ⇒ DefaultCommandPolicy + shell on. We assert the policy by behaviour
	// (a write command denied/allowed) rather than struct identity.
	writeCmd := "echo hi > out.txt"
	cases := []struct {
		mode         string
		wantReadOnly bool
	}{
		{"discuss", true},
		{"plan", true},
		{"execute", false},
		{"auto", false},
	}
	for _, c := range cases {
		d, err := capability.For(capability.Request{Mode: c.mode})
		if err != nil {
			t.Fatalf("For(%q): %v", c.mode, err)
		}
		if d.Tools.ReadOnly != c.wantReadOnly {
			t.Errorf("For(%q).Tools.ReadOnly = %v, want %v", c.mode, d.Tools.ReadOnly, c.wantReadOnly)
		}
		if d.ShellEnabled != !c.wantReadOnly {
			t.Errorf("For(%q).ShellEnabled = %v, want %v", c.mode, d.ShellEnabled, !c.wantReadOnly)
		}
		// command-policy parity: a write redirection is denied iff read-only.
		gotAllowed, _ := d.CommandPolicy.Check(writeCmd)
		wantAllowed, _ := legacyPolicy(c.wantReadOnly).Check(writeCmd)
		if gotAllowed != wantAllowed {
			t.Errorf("For(%q).CommandPolicy allows %q = %v, want %v", c.mode, writeCmd, gotAllowed, wantAllowed)
		}
	}
}

func legacyPolicy(readOnly bool) policy.CommandPolicy {
	if readOnly {
		return policy.ReadOnlyCommandPolicy()
	}
	return policy.DefaultCommandPolicy()
}

func TestForEgressDefaultDenyAndProfile(t *testing.T) {
	// no profile ⇒ deny-all egress.
	d, err := capability.For(capability.Request{Mode: "execute"})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if !d.Egress.Empty() {
		t.Errorf("no profile should be deny-all egress, got %v", d.Egress.Allowed)
	}

	// a named profile ⇒ a non-empty allowlist + recorded source labels.
	dp, err := capability.For(capability.Request{Mode: "execute", ProfileName: "browse"})
	if err != nil {
		t.Fatalf("For(browse): %v", err)
	}
	if dp.Egress.Empty() {
		t.Errorf("browse profile should resolve a non-empty allowlist")
	}
	if len(dp.EgressSources) == 0 {
		t.Errorf("a resolved profile should record its source labels")
	}
}

func TestEvaluateMatchesCapguard(t *testing.T) {
	// non-trifecta (chat-like: no untrusted input) ⇒ Allow regardless of gate.
	chat, _ := capability.For(capability.Request{Mode: "execute", ReadRepo: true, GateAvailable: false})
	if v := chat.Evaluate().Verdict; v != capguard.Allow {
		t.Errorf("chat-like Evaluate = %v, want Allow (axis A off ⇒ no trifecta)", v)
	}

	// trifecta present + gate ⇒ GateRequired; no gate ⇒ Refuse (fail closed).
	rule := capguard.Capabilities{UntrustedInput: true, PrivateData: true, OpenEgress: true}
	gated := capability.Descriptor{Rule: rule, Approver: true}
	if v := gated.Evaluate().Verdict; v != capguard.GateRequired {
		t.Errorf("trifecta+gate Evaluate = %v, want GateRequired", v)
	}
	headless := capability.Descriptor{Rule: rule, Approver: false}
	if v := headless.Evaluate().Verdict; v != capguard.Refuse {
		t.Errorf("trifecta+no-gate Evaluate = %v, want Refuse", v)
	}
	// parity with calling capguard directly (Decision has a slice field, so
	// compare the verdict).
	if gated.Evaluate().Verdict != capguard.Evaluate(rule, true).Verdict {
		t.Errorf("Descriptor.Evaluate must equal capguard.Evaluate(Rule, Approver)")
	}
}

func TestEventIsMetadataOnly(t *testing.T) {
	// a browse-like descriptor reaching real hosts: the event must carry source
	// LABELS and a verdict label, never the host allowlist or a secret.
	d, err := capability.For(capability.Request{
		Mode: "execute", ReadRepo: true, UntrustedInput: true, ProfileName: "browse", GateAvailable: true,
	})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	ev := d.Event()
	for _, k := range []string{"mode", "read_only", "shell", "egress_sources", "verdict", "axes"} {
		if _, ok := ev[k]; !ok {
			t.Errorf("event missing key %q", k)
		}
	}
	if _, leaks := ev["egress_hosts"]; leaks {
		t.Errorf("event must not carry the host allowlist")
	}
	// no resolved host string should appear anywhere in the event values.
	for _, host := range d.Egress.Allowed {
		if host == "" {
			continue
		}
		if strings.Contains(strings.ToLower(renderValues(ev)), strings.ToLower(host)) {
			t.Errorf("event leaks egress host %q", host)
		}
	}
	if v, ok := ev["verdict"].(string); !ok || v == "" {
		t.Errorf("verdict should be a non-empty label string, got %v", ev["verdict"])
	}
}

// renderValues flattens an event map's scalar/string-slice values to one string
// for a leak scan.
func renderValues(m map[string]any) string {
	var b strings.Builder
	for _, v := range m {
		switch t := v.(type) {
		case string:
			b.WriteString(t)
			b.WriteByte(' ')
		case []string:
			b.WriteString(strings.Join(t, " "))
			b.WriteByte(' ')
		}
	}
	return b.String()
}
