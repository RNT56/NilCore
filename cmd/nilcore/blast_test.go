package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"nilcore/internal/blastbudget"
	"nilcore/internal/eventlog"
)

// TestXC04_BlastDayWindowRebuildsFromLog proves the per-UTC-day auto-approval $ ceiling
// REBUILDS from the durable log on boot, so a process restart cannot reset the fence
// (no fail-open on restart — the I5 rebuild-on-boot discipline).
func TestXC04_BlastDayWindowRebuildsFromLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "e.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// A prior auto_approve TODAY charged $20 (the exact shape graapprove emits).
	log.Append(eventlog.Event{Kind: "auto_approve", Detail: map[string]any{
		"action":  "deploy",
		"scope":   "staging",
		"dollars": map[string]any{"charged": 20.0, "max_dollars_day": 25.0},
	}})
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	// A FRESH budget (simulating a restart) must rebuild the day window so the $25/day
	// ceiling already reflects the prior $20.
	reopened, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	b := blastbudget.New()
	b.SetAutoApprovalDollarCeiling(25)
	rebuildBlastDay(b, reopened.Path())

	today := time.Now().UTC().Format("2006-01-02")
	if u := b.Used(today); u.Dollars != 20 {
		t.Fatalf("rebuilt day window = $%.2f, want $20 from the log", u.Dollars)
	}
	// Only $5 remains: a further $6 must breach (the restart did NOT reset the fence).
	if err := b.ChargeAutoApprovalDollars(context.Background(), today, 6); err == nil {
		t.Fatal("restart must not reset the $ window — $20 prior + $6 must breach $25")
	}
}

// TestRebuildBlastDaySumsActualSpend proves the per-day $ accounting sums the ACTUAL
// charged spend, not the clause ceiling: it prefers the explicit dollars.actual_usd
// field (the distinct actual-cost value graapprove now emits) and, for logs written
// before that field existed, falls back to dollars.charged — which also carries the
// actual amount after the actual-spend fix. Mixing the two must sum to the real total.
func TestRebuildBlastDaySumsActualSpend(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "e.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// Event A: new shape — actual_usd=2, charged=2, ceiling=5. Must count $2 (actual).
	log.Append(eventlog.Event{Kind: "auto_approve", Detail: map[string]any{
		"action": "deploy", "scope": "staging",
		"dollars": map[string]any{"charged": 2.0, "actual_usd": 2.0, "max_dollars_day": 5.0},
	}})
	// Event B: legacy shape — only charged=3 present (no actual_usd). Falls back to $3.
	log.Append(eventlog.Event{Kind: "auto_approve", Detail: map[string]any{
		"action": "deploy", "scope": "staging",
		"dollars": map[string]any{"charged": 3.0, "max_dollars_day": 5.0},
	}})
	// A non-auto_approve event must contribute nothing.
	log.Append(eventlog.Event{Kind: "boundary_outcome", Detail: map[string]any{
		"action": "deploy", "scope": "staging", "passed": true,
	}})
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	b := blastbudget.New()
	b.SetAutoApprovalDollarCeiling(100)
	rebuildBlastDay(b, reopened.Path())

	today := time.Now().UTC().Format("2006-01-02")
	if u := b.Used(today); u.Dollars != 5 {
		t.Fatalf("rebuilt day window = $%.2f, want $5 (actual_usd 2 + charged 3)", u.Dollars)
	}
}

// TestAutoApprovalActualUSD pins the single field-selection helper: actual_usd wins when
// present; charged is the fallback; a missing/non-float value contributes 0.
func TestAutoApprovalActualUSD(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want float64
	}{
		{"actual_usd preferred", map[string]any{"charged": 9.0, "actual_usd": 2.0}, 2},
		{"charged fallback", map[string]any{"charged": 3.0}, 3},
		{"empty is zero", map[string]any{}, 0},
		{"non-float is zero", map[string]any{"charged": "nope"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := autoApprovalActualUSD(tc.in); got != tc.want {
				t.Fatalf("autoApprovalActualUSD(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestMintBlastBudget_OffIsNil(t *testing.T) {
	// "off" / empty ⇒ no budget (unfenced, byte-identical default-off).
	for _, p := range []string{"off", ""} {
		if b := mintBlastBudget(p, nil); b != nil {
			t.Errorf("mintBlastBudget(%q) = non-nil, want nil (no fence)", p)
		}
	}
}

// TestMintBlastBudget_UnknownFailsClosed proves a typo on the safety flag does NOT
// silently disable the fence (the old fail-open bug): it falls back to the tightest
// envelope instead of returning nil/unfenced.
func TestMintBlastBudget_UnknownFailsClosed(t *testing.T) {
	b := mintBlastBudget("standrd", nil) // typo of "standard"
	if b == nil {
		t.Fatal("unknown -blast-radius must fail CLOSED (tight), not unfenced (nil)")
	}
	u := b.Used("2026-06-26")
	tight := blastPresets["tight"]
	if u.HostCeiling != tight.hosts || u.IrrevCeiling != tight.irrev || u.DayCeiling != tight.dollarsDay {
		t.Errorf("unknown value must fall back to the tight envelope, got %+v", u)
	}
}

func TestMintBlastBudget_PresetCeilings(t *testing.T) {
	b := mintBlastBudget("standard", nil)
	if b == nil {
		t.Fatal("standard should mint a budget")
	}
	u := b.Used("2026-06-26")
	if u.HostCeiling != 8 || u.IrrevCeiling != 5 || u.WallCeiling != 20*time.Minute || u.DayCeiling != 5 {
		t.Errorf("standard ceilings = %+v, want hosts=8 irrev=5 wall=20m day=$5", u)
	}
	// No preset leaves any axis unbounded (every ceiling must be positive).
	for _, name := range []string{"tight", "standard"} {
		uu := mintBlastBudget(name, nil).Used("d")
		if uu.HostCeiling <= 0 || uu.IrrevCeiling <= 0 || uu.WallCeiling <= 0 || uu.DayCeiling <= 0 {
			t.Errorf("preset %q has an unbounded axis: %+v", name, uu)
		}
	}
}
