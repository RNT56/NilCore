package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
)

type fakeApprover struct {
	ok    bool
	calls int
}

func (f *fakeApprover) Approve(string) bool { f.calls++; return f.ok }

// TestEnforceRuleOfTwo pins the trifecta gate on the main paths: a narrow/empty egress
// (the shipped default) is Allow and never prompts (byte-identical), while the lethal
// trifecta (web + repo + WIDE egress) routes through the gate — attended approves,
// attended denies aborts, headless-no-gate refuses — and the opt-out never blocks.
func TestEnforceRuleOfTwo(t *testing.T) {
	narrow := policy.Egress{Allowed: []string{"api.example.com"}} // 1 host, no wildcard → not open
	wide := policy.Egress{Allowed: []string{"*.example.com"}}     // wildcard → open egress (axis C)
	deny := policy.Egress{}                                       // deny-all → axis A + C false

	t.Run("narrow egress => Allow, no prompt, no block", func(t *testing.T) {
		ap := &fakeApprover{ok: false}
		if err := enforceRuleOfTwo(nil, true, true, true, narrow, ap, "p"); err != nil {
			t.Fatalf("narrow egress must Allow (byte-identical), got %v", err)
		}
		if ap.calls != 0 {
			t.Fatalf("narrow egress must not reach the gate, calls=%d", ap.calls)
		}
	})

	t.Run("deny-all egress => Allow (axis A and C both false)", func(t *testing.T) {
		ap := &fakeApprover{ok: false}
		if err := enforceRuleOfTwo(nil, true, false, true, deny, ap, "p"); err != nil {
			t.Fatalf("deny-all must Allow, got %v", err)
		}
		if ap.calls != 0 {
			t.Fatalf("deny-all must not reach the gate, calls=%d", ap.calls)
		}
	})

	t.Run("wide trifecta + attended approve => proceed", func(t *testing.T) {
		ap := &fakeApprover{ok: true}
		if err := enforceRuleOfTwo(nil, true, true, true, wide, ap, "p"); err != nil {
			t.Fatalf("approved trifecta must proceed, got %v", err)
		}
		if ap.calls != 1 {
			t.Fatalf("trifecta must prompt exactly once, calls=%d", ap.calls)
		}
	})

	t.Run("wide trifecta + attended deny => error", func(t *testing.T) {
		ap := &fakeApprover{ok: false}
		if err := enforceRuleOfTwo(nil, true, true, true, wide, ap, "p"); err == nil {
			t.Fatal("denied trifecta must abort")
		}
		if ap.calls != 1 {
			t.Fatalf("trifecta must prompt exactly once, calls=%d", ap.calls)
		}
	})

	t.Run("wide trifecta + headless (nil gate) => refuse, fail closed", func(t *testing.T) {
		if err := enforceRuleOfTwo(nil, true, true, true, wide, nil, "p"); err == nil {
			t.Fatal("headless trifecta with no gate must be refused (fail closed)")
		}
	})

	t.Run("opt-out (enforce=false) => never blocks even on the trifecta", func(t *testing.T) {
		if err := enforceRuleOfTwo(nil, false, true, true, wide, nil, "p"); err != nil {
			t.Fatalf("enforce=false must never block, got %v", err)
		}
	})

	t.Run("event is metadata-only: verdict+axes, never the host list", func(t *testing.T) {
		dir := t.TempDir()
		lp := filepath.Join(dir, "e.jsonl")
		log, err := eventlog.Open(lp)
		if err != nil {
			t.Fatal(err)
		}
		secretHost := "exfil-sink-999.example.net"
		_ = enforceRuleOfTwo(log, true, true, true, policy.Egress{Allowed: []string{"*." + secretHost}}, &fakeApprover{ok: true}, "p")
		_ = log.Close()
		body, _ := os.ReadFile(lp)
		s := string(body)
		if !strings.Contains(s, "capguard") || !strings.Contains(s, "\"verdict\"") {
			t.Fatalf("expected a capguard verdict event, got:\n%s", s)
		}
		if strings.Contains(s, secretHost) {
			t.Fatalf("the resolved egress host list leaked into the append-only log (I3/I7):\n%s", s)
		}
	})
}
