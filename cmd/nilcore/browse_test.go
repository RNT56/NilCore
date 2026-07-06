package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nilcore/internal/browseragent"
	"nilcore/internal/browserwire"
)

// fakeBrowseSess is a minimal browseragent.Session for the wiring test: it records the
// acts it receives and reports a "Pay now" button as the latest snapshot so the browse
// tool classifies a click on it as irreversible.
type fakeBrowseSess struct {
	got    []browserwire.Act
	latest browserwire.Observation
}

func (f *fakeBrowseSess) Act(_ context.Context, a browserwire.Act) (browserwire.Observation, error) {
	f.got = append(f.got, a)
	return f.latest, nil
}
func (f *fakeBrowseSess) Latest() browserwire.Observation { return f.latest }

// recordingApprover captures the gate prompt and returns a fixed verdict.
type recordingApprover struct {
	verdict bool
	prompts []string
}

func (r *recordingApprover) Approve(action string) bool {
	r.prompts = append(r.prompts, action)
	return r.verdict
}

// TestBuildBrowseToolWiresApprover proves the production wiring fix: the browse tool
// built by buildBrowseTool routes an irreversible action through the injected Approver
// instead of failing closed. A tool built WITHOUT an Approver (the pre-fix bug) would
// dead-block every irreversible action permanently. We assert both halves: with an
// approving gate the click reaches the session; with a nil approver it fails closed.
func TestBuildBrowseToolWiresApprover(t *testing.T) {
	latest := browserwire.Observation{
		Version: 1,
		Refs:    []browserwire.Ref{{ID: 1, Role: "button", Name: "Pay now", Version: 1}},
	}
	click, _ := json.Marshal(map[string]any{"op": "click", "ref": 1})

	// (a) Approver present + approves → the irreversible click is gated, then performed.
	sess := &fakeBrowseSess{latest: latest}
	appr := &recordingApprover{verdict: true}
	bt := buildBrowseTool(sess, 10, nil, appr)
	if bt.Approver == nil {
		t.Fatal("buildBrowseTool must wire the Approver (nil ⇒ every irreversible action dead-blocked)")
	}
	if _, _, err := bt.RunWithImage(context.Background(), ".", click); err != nil {
		t.Fatalf("RunWithImage: %v", err)
	}
	if len(appr.prompts) != 1 || !strings.Contains(appr.prompts[0], "pay") {
		t.Fatalf("the irreversible click must route through the approver, prompts=%v", appr.prompts)
	}
	if len(sess.got) != 1 {
		t.Fatalf("an approved irreversible action must reach the session, got %d acts", len(sess.got))
	}

	// (b) No Approver → the same click fails closed (blocked, never performed) — the
	// pre-fix production behavior, shown here to be the failure mode the wiring avoids.
	sess2 := &fakeBrowseSess{latest: latest}
	bt2 := buildBrowseTool(sess2, 10, nil, nil)
	out, _, err := bt2.RunWithImage(context.Background(), ".", click)
	if err != nil {
		t.Fatalf("RunWithImage(no approver): %v", err)
	}
	if !strings.Contains(out, "BLOCKED") {
		t.Fatalf("a nil-approver irreversible click must be blocked, got %q", out)
	}
	if len(sess2.got) != 0 {
		t.Fatal("a blocked irreversible action must never reach the session")
	}
}

// ensure the console-approver type satisfies the browse Approver seam (compile guard).
var _ browseragent.Approver = (*recordingApprover)(nil)
