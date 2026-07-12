package desktopagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nilcore/internal/desktopwire"
)

type fakeSession struct {
	got    []desktopwire.Act
	respFn func(a desktopwire.Act) (desktopwire.Observation, error)
	latest desktopwire.Observation
}

func (f *fakeSession) Act(_ context.Context, a desktopwire.Act) (desktopwire.Observation, error) {
	f.got = append(f.got, a)
	if f.respFn != nil {
		o, err := f.respFn(a)
		f.latest = o
		return o, err
	}
	f.latest = desktopwire.Observation{Version: 1, Rung: desktopwire.RungATSPI}
	return f.latest, nil
}
func (f *fakeSession) Latest() desktopwire.Observation { return f.latest }

func run(t *testing.T, c *ComputerTool, m map[string]any) (string, bool) {
	t.Helper()
	in, _ := json.Marshal(m)
	out, img, err := c.RunWithImage(context.Background(), ".", in)
	if err != nil {
		t.Fatalf("RunWithImage(%v): %v", m, err)
	}
	return out, img != nil
}

func TestRendersRefsFenced(t *testing.T) {
	fs := &fakeSession{respFn: func(a desktopwire.Act) (desktopwire.Observation, error) {
		return desktopwire.Observation{Version: 1, Rung: desktopwire.RungATSPI, FocusedWindow: "Settings",
			Refs: []desktopwire.Ref{{ID: 0, Role: "push button", Name: "Save"}}}, nil
	}}
	c := &ComputerTool{Sess: fs}
	out, _ := run(t, c, map[string]any{"op": "observe"})
	for _, want := range []string{"Settings", "Save", "[0]", "accessibility refs"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
}

func TestScreenshotImageWhenNoRefs(t *testing.T) {
	fs := &fakeSession{respFn: func(a desktopwire.Act) (desktopwire.Observation, error) {
		return desktopwire.Observation{Version: 1, Rung: desktopwire.RungCoordinate, ScreenshotB64: "QUJD"}, nil
	}}
	c := &ComputerTool{Sess: fs}
	out, hasImg := run(t, c, map[string]any{"op": "observe"})
	if !hasImg {
		t.Fatal("a screenshot observation must return an image")
	}
	if !strings.Contains(out, "coordinate") {
		t.Fatalf("rung-3 render should mention coordinate: %s", out)
	}
}

func TestBudgetEnforced(t *testing.T) {
	fs := &fakeSession{respFn: func(a desktopwire.Act) (desktopwire.Observation, error) {
		return desktopwire.Observation{Version: 1, FocusedWindow: a.Key + a.Op}, nil
	}}
	c := &ComputerTool{Sess: fs, MaxSteps: 2}
	for i := 0; i < 2; i++ {
		run(t, c, map[string]any{"op": "key", "key": string(rune('a' + i))})
	}
	before := len(fs.got)
	out, _ := run(t, c, map[string]any{"op": "observe"})
	if !strings.Contains(out, "budget") {
		t.Fatalf("expected budget message, got %s", out)
	}
	if len(fs.got) != before {
		t.Fatal("over-budget act reached the session")
	}
}

func TestStagnation(t *testing.T) {
	fs := &fakeSession{respFn: func(a desktopwire.Act) (desktopwire.Observation, error) {
		return desktopwire.Observation{Version: 1, FocusedWindow: "same", Title: "same"}, nil
	}}
	c := &ComputerTool{Sess: fs, MaxStagnant: 2}
	var last string
	for i := 0; i < 4; i++ {
		last, _ = run(t, c, map[string]any{"op": "click", "coordinate": []int{1, 1}})
	}
	if !strings.Contains(last, "different approach") {
		t.Fatalf("expected stagnation nudge, got %s", last)
	}
}

// recordingApprover captures the gate prompt and returns a fixed verdict.
type recordingApprover struct {
	verdict bool
	prompts []string
}

func (r *recordingApprover) Approve(action string) bool {
	r.prompts = append(r.prompts, action)
	return r.verdict
}

// TestIrreversibleGateBlocksWithoutApprover proves the in-code gate (I2): a click on a
// ref whose accessible name names a destructive action ("Delete account") is BLOCKED when
// there is no Approver — it never reaches the session, mirroring the browser tier's
// deny-default-headless behavior.
func TestIrreversibleGateBlocksWithoutApprover(t *testing.T) {
	fs := &fakeSession{}
	fs.latest = desktopwire.Observation{Version: 1, Rung: desktopwire.RungATSPI,
		Refs: []desktopwire.Ref{{ID: 5, Role: "push button", Name: "Delete account", Version: 1}}}
	c := &ComputerTool{Sess: fs} // no Approver ⇒ fail closed
	out, _ := run(t, c, map[string]any{"op": "click", "ref": 5})
	if !strings.Contains(out, "BLOCKED") || !strings.Contains(out, "delete") {
		t.Fatalf("irreversible click must be blocked without an approver, got %q", out)
	}
	if len(fs.got) != 0 {
		t.Fatal("a blocked irreversible click must never reach the session")
	}
}

// TestIrreversibleGateRoutesToApprover proves an approving gate lets the action through,
// and a denying gate blocks it — the click is classified against the ref's name.
func TestIrreversibleGateRoutesToApprover(t *testing.T) {
	latest := desktopwire.Observation{Version: 1, Rung: desktopwire.RungATSPI,
		Refs: []desktopwire.Ref{{ID: 5, Role: "push button", Name: "Pay now", Version: 1}}}

	// Approves → the click reaches the session.
	fs := &fakeSession{}
	fs.latest = latest
	appr := &recordingApprover{verdict: true}
	c := &ComputerTool{Sess: fs, Approver: appr}
	run(t, c, map[string]any{"op": "click", "ref": 5})
	if len(appr.prompts) != 1 || !strings.Contains(appr.prompts[0], "pay") {
		t.Fatalf("the irreversible click must route through the approver, prompts=%v", appr.prompts)
	}
	if len(fs.got) != 1 {
		t.Fatalf("an approved irreversible click must reach the session, got %d", len(fs.got))
	}

	// Denies → blocked, never dispatched.
	fs2 := &fakeSession{}
	fs2.latest = latest
	deny := &recordingApprover{verdict: false}
	c2 := &ComputerTool{Sess: fs2, Approver: deny}
	out, _ := run(t, c2, map[string]any{"op": "click", "ref": 5})
	if !strings.Contains(out, "BLOCKED") {
		t.Fatalf("a denied irreversible click must be blocked, got %q", out)
	}
	if len(fs2.got) != 0 {
		t.Fatal("a denied irreversible click must never reach the session")
	}
}

// TestBenignActionUngated confirms an ordinary click (a non-consequential ref) is never
// gated — the classifier does not over-block. A coordinate click (no accessible target)
// is likewise ungated.
func TestBenignActionUngated(t *testing.T) {
	fs := &fakeSession{}
	fs.latest = desktopwire.Observation{Version: 1, Rung: desktopwire.RungATSPI,
		Refs: []desktopwire.Ref{{ID: 5, Role: "push button", Name: "Open folder", Version: 1}}}
	c := &ComputerTool{Sess: fs} // no approver, but no gate should fire
	run(t, c, map[string]any{"op": "click", "ref": 5})
	if len(fs.got) != 1 {
		t.Fatalf("a benign click must not be gated, reached session %d times", len(fs.got))
	}
	// A coordinate click on the same (benign) screen also stays ungated.
	run(t, c, map[string]any{"op": "click", "coordinate": []int{10, 20}})
	if len(fs.got) != 2 {
		t.Fatal("a coordinate click has no accessible target and must not be gated")
	}
}

// TestIrreversibleSignalWordBoundary proves the classifier matches on WORD boundaries,
// so a signal never fires inside a benign longer word — the regression where "format"
// matched "information" and "send" matched "sender"/"resend", which (deny-default
// headless) would permanently block benign screens. The real destructive labels, where
// the word appears standalone, must still match.
func TestIrreversibleSignalWordBoundary(t *testing.T) {
	benign := []string{
		"contact information", "more information", "payment information",
		"resend code", "message from sender", "displayed",
	}
	for _, s := range benign {
		if sig := irreversibleSignal(s); sig != "" {
			t.Errorf("benign text %q must not match an irreversible signal, got %q", s, sig)
		}
	}
	// The real destructive labels still match (the word stands alone).
	gated := map[string]string{
		"Format Disk":    "format",
		"Send":           "send",
		"Delete Account": "delete",
		"Buy now":        "buy now",
		"Shut Down":      "shut down",
	}
	for text, want := range gated {
		if sig := irreversibleSignal(text); sig != want {
			t.Errorf("text %q must gate on %q, got %q", text, want, sig)
		}
	}
}

// TestSubmitKeyGatedOnConsequentialWindow proves an Enter/Return is gated when the window
// names an irreversible action, mirroring the browser tier's Enter-to-submit rule.
func TestSubmitKeyGatedOnConsequentialWindow(t *testing.T) {
	fs := &fakeSession{}
	fs.latest = desktopwire.Observation{Version: 1, Rung: desktopwire.RungATSPI,
		FocusedWindow: "Confirm purchase", Title: "Confirm purchase",
		Refs: []desktopwire.Ref{{ID: 1, Role: "push button", Name: "OK", Version: 1}}}
	c := &ComputerTool{Sess: fs} // no approver ⇒ blocked
	out, _ := run(t, c, map[string]any{"op": "key", "key": "Return"})
	if !strings.Contains(out, "BLOCKED") {
		t.Fatalf("Enter on a consequential window must be gated, got %q", out)
	}
	if len(fs.got) != 0 {
		t.Fatal("a blocked submit must never reach the session")
	}
	// A non-submit key (Tab) on the same window is benign and ungated.
	run(t, c, map[string]any{"op": "key", "key": "Tab"})
	if len(fs.got) != 1 {
		t.Fatal("a non-submit key must not be gated")
	}
}

// TestHostGateAllMutations proves FIX 3: on the host-control tier (GateAllMutations=true)
// EVERY mutating action is routed through the approver — not just irreversibleTarget
// matches. On the REAL desktop the CV-only observation carries no accessible names, so
// irreversibleTarget is structurally blind and would gate NOTHING; GateAllMutations is what
// makes the per-action gate real. A deny-approver blocks a plain, benign click and it never
// reaches the session; read-only observe stays ungated; and contained mode
// (GateAllMutations=false) leaves the same click ungated (unchanged behavior).
func TestHostGateAllMutations(t *testing.T) {
	// A benign, structure-less screen (host CV mode): a coordinate click with no accessible
	// target — irreversibleTarget returns "" so contained mode would let it through.
	latest := desktopwire.Observation{Version: 1, Rung: desktopwire.RungCoordinate}

	// Host mode + deny approver ⇒ the plain click is BLOCKED, never dispatched.
	fs := &fakeSession{}
	fs.latest = latest
	deny := &recordingApprover{verdict: false}
	c := &ComputerTool{Sess: fs, Approver: deny, GateAllMutations: true}
	out, _ := run(t, c, map[string]any{"op": "click", "coordinate": []int{10, 20}})
	if !strings.Contains(out, "BLOCKED") {
		t.Fatalf("a host-mode click must be gated, got %q", out)
	}
	if len(fs.got) != 0 {
		t.Fatal("a denied host-mode click must never reach the session")
	}
	if len(deny.prompts) != 1 {
		t.Fatalf("the click must route through the approver, prompts=%v", deny.prompts)
	}

	// A read-only observe is never gated, even in host mode.
	run(t, c, map[string]any{"op": "observe"})
	if len(fs.got) != 1 {
		t.Fatal("observe is read-only and must not be gated in host mode")
	}

	// Contained mode (GateAllMutations=false): the same benign coordinate click is ungated.
	fs2 := &fakeSession{}
	fs2.latest = latest
	c2 := &ComputerTool{Sess: fs2, Approver: &recordingApprover{verdict: false}, GateAllMutations: false}
	run(t, c2, map[string]any{"op": "click", "coordinate": []int{10, 20}})
	if len(fs2.got) != 1 {
		t.Fatal("contained-mode benign click must NOT be gated (unchanged behavior)")
	}
}

func TestEventSink(t *testing.T) {
	var steps []Step
	fs := &fakeSession{respFn: func(a desktopwire.Act) (desktopwire.Observation, error) {
		return desktopwire.Observation{Version: 2, Rung: desktopwire.RungATSPI, FocusedWindow: "calc",
			Refs: []desktopwire.Ref{{ID: 0}}}, nil
	}}
	c := &ComputerTool{Sess: fs, EventSink: func(s Step) { steps = append(steps, s) }}
	run(t, c, map[string]any{"op": "click", "ref": 0})
	if len(steps) != 1 || steps[0].Op != "click" || steps[0].Window != "calc" || steps[0].Rung != desktopwire.RungATSPI {
		t.Fatalf("trajectory step wrong: %+v", steps)
	}
}
