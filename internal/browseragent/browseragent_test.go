package browseragent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nilcore/internal/browserwire"
)

// fakeSession is a scriptable Session for hermetic tool tests (no daemon, no Chrome).
type fakeSession struct {
	got    []browserwire.Act
	respFn func(a browserwire.Act) (browserwire.Observation, error)
	latest browserwire.Observation
}

func (f *fakeSession) Act(_ context.Context, a browserwire.Act) (browserwire.Observation, error) {
	f.got = append(f.got, a)
	if f.respFn != nil {
		obs, err := f.respFn(a)
		f.latest = obs
		return obs, err
	}
	f.latest = browserwire.Observation{Version: 1, URL: "http://x.test/"}
	return f.latest, nil
}
func (f *fakeSession) Latest() browserwire.Observation { return f.latest }

func runOp(t *testing.T, b *BrowseTool, op string) (string, bool) {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"op": op})
	out, img, err := b.RunWithImage(context.Background(), ".", in)
	if err != nil {
		t.Fatalf("RunWithImage(%s): %v", op, err)
	}
	return out, img != nil
}

func TestRendersObservationFenced(t *testing.T) {
	fs := &fakeSession{respFn: func(a browserwire.Act) (browserwire.Observation, error) {
		return browserwire.Observation{
			Version: 1, URL: "http://x.test/", Title: "Hello",
			Refs: []browserwire.Ref{{ID: 0, Role: "button", Name: "Submit"}},
			Text: "hello world",
		}, nil
	}}
	b := &BrowseTool{Sess: fs}
	out, _ := runOp(t, b, "observe")
	for _, want := range []string{"http://x.test/", "Submit", "[0]", "hello world"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered observation missing %q: %s", want, out)
		}
	}
}

func TestScreenshotPassedAsImageWhenNoRefs(t *testing.T) {
	fs := &fakeSession{respFn: func(a browserwire.Act) (browserwire.Observation, error) {
		return browserwire.Observation{Version: 1, ScreenshotB64: "QUJD"}, nil
	}}
	b := &BrowseTool{Sess: fs}
	_, hasImg := runOp(t, b, "observe")
	if !hasImg {
		t.Fatal("a screenshot-bearing observation must return an image")
	}
}

func TestBudgetIsEnforced(t *testing.T) {
	fs := &fakeSession{respFn: func(a browserwire.Act) (browserwire.Observation, error) {
		// Each act lands on a NEW url so stagnation never trips before the budget.
		return browserwire.Observation{Version: 1, URL: "http://x.test/" + a.Op + a.URL}, nil
	}}
	b := &BrowseTool{Sess: fs, MaxSteps: 3}
	for i := 0; i < 3; i++ {
		in, _ := json.Marshal(map[string]any{"op": "navigate", "url": string(rune('a' + i))})
		if _, _, err := b.RunWithImage(context.Background(), ".", in); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	// The 4th call exceeds the budget and must return the budget message without
	// dispatching to the session.
	before := len(fs.got)
	out, _ := runOp(t, b, "observe")
	if !strings.Contains(out, "budget") {
		t.Fatalf("expected a budget message after the cap, got %q", out)
	}
	if len(fs.got) != before {
		t.Fatal("an over-budget act must not reach the session")
	}
}

func TestStagnationDetected(t *testing.T) {
	// The session always returns the SAME signature for a mutating act → stagnant.
	fs := &fakeSession{respFn: func(a browserwire.Act) (browserwire.Observation, error) {
		return browserwire.Observation{Version: 1, URL: "http://same/", Title: "same"}, nil
	}}
	b := &BrowseTool{Sess: fs, MaxStagnant: 2}
	var last string
	for i := 0; i < 4; i++ {
		in, _ := json.Marshal(map[string]any{"op": "click", "ref": 1})
		// ref 1 isn't validated here (fake session ignores it); we exercise the tool.
		out, _, err := b.RunWithImage(context.Background(), ".", in)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		last = out
	}
	if !strings.Contains(last, "different approach") {
		t.Fatalf("expected a stagnation nudge after repeated no-ops, got %q", last)
	}
}

func TestForwardsActFields(t *testing.T) {
	fs := &fakeSession{}
	b := &BrowseTool{Sess: fs}
	in, _ := json.Marshal(map[string]any{"op": "type", "ref": 5, "text": "{{secret:pw}}"})
	if _, _, err := b.RunWithImage(context.Background(), ".", in); err != nil {
		t.Fatal(err)
	}
	if len(fs.got) != 1 || fs.got[0].Op != "type" || fs.got[0].Ref != 5 || fs.got[0].Text != "{{secret:pw}}" {
		t.Fatalf("act not forwarded verbatim (secret substitution is the session's job): %+v", fs.got)
	}
}

func TestEventSinkEmitsTrajectoryStep(t *testing.T) {
	var steps []Step
	fs := &fakeSession{respFn: func(a browserwire.Act) (browserwire.Observation, error) {
		return browserwire.Observation{Version: 2, URL: "http://x.test/" + a.Op, Refs: []browserwire.Ref{{ID: 0}}}, nil
	}}
	b := &BrowseTool{Sess: fs, EventSink: func(s Step) { steps = append(steps, s) }}
	if _, _, err := b.RunWithImage(context.Background(), ".", mustInput("navigate", "http://x.test/")); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("expected 1 trajectory step, got %d", len(steps))
	}
	s := steps[0]
	if s.Op != "navigate" || s.Refs != 1 || s.Version != 2 || s.N != 1 {
		t.Fatalf("step metadata wrong: %+v", s)
	}
}

func mustInput(op, url string) []byte {
	b, _ := json.Marshal(map[string]any{"op": op, "url": url})
	return b
}

// recordingApprover records each gated action and returns a fixed verdict.
type recordingApprover struct {
	verdict bool
	asked   []string
}

func (r *recordingApprover) Approve(action string) bool {
	r.asked = append(r.asked, action)
	return r.verdict
}

func TestIrreversibleClickFailsClosedWithoutApprover(t *testing.T) {
	fs := &fakeSession{}
	// Seed the latest snapshot so the click ref resolves to a "Pay now" button.
	fs.latest = browserwire.Observation{Version: 1, Refs: []browserwire.Ref{{ID: 1, Role: "button", Name: "Pay now", Version: 1}}}
	b := &BrowseTool{Sess: fs} // nil Approver ⇒ fail closed
	in, _ := json.Marshal(map[string]any{"op": "click", "ref": 1})
	out, _, err := b.RunWithImage(context.Background(), ".", in)
	if err != nil {
		t.Fatalf("RunWithImage: %v", err)
	}
	if !strings.Contains(out, "BLOCKED") {
		t.Fatalf("an irreversible click with no approver must be blocked, got %q", out)
	}
	if len(fs.got) != 0 {
		t.Fatal("a blocked irreversible action must never reach the session")
	}
}

func TestIrreversibleClickDeniedAtGate(t *testing.T) {
	fs := &fakeSession{}
	fs.latest = browserwire.Observation{Version: 1, Refs: []browserwire.Ref{{ID: 2, Role: "button", Name: "Delete account", Version: 1}}}
	appr := &recordingApprover{verdict: false}
	b := &BrowseTool{Sess: fs, Approver: appr}
	in, _ := json.Marshal(map[string]any{"op": "click", "ref": 2})
	out, _, err := b.RunWithImage(context.Background(), ".", in)
	if err != nil {
		t.Fatalf("RunWithImage: %v", err)
	}
	if len(appr.asked) != 1 {
		t.Fatalf("the approver must be consulted exactly once, got %d", len(appr.asked))
	}
	if !strings.Contains(out, "BLOCKED") || len(fs.got) != 0 {
		t.Fatalf("a denied irreversible click must not be dispatched; out=%q sent=%d", out, len(fs.got))
	}
}

func TestIrreversibleClickApprovedDispatches(t *testing.T) {
	fs := &fakeSession{respFn: func(a browserwire.Act) (browserwire.Observation, error) {
		return browserwire.Observation{Version: 2, URL: "http://x.test/paid"}, nil
	}}
	fs.latest = browserwire.Observation{Version: 1, Refs: []browserwire.Ref{{ID: 3, Role: "button", Name: "Confirm purchase", Version: 1}}}
	appr := &recordingApprover{verdict: true}
	b := &BrowseTool{Sess: fs, Approver: appr}
	in, _ := json.Marshal(map[string]any{"op": "click", "ref": 3})
	if _, _, err := b.RunWithImage(context.Background(), ".", in); err != nil {
		t.Fatalf("RunWithImage: %v", err)
	}
	if len(appr.asked) != 1 {
		t.Fatalf("the approver must be consulted, got %d", len(appr.asked))
	}
	if len(fs.got) != 1 {
		t.Fatal("an approved irreversible click must be dispatched to the session")
	}
}

// TestEnterSubmitOnIrreversiblePageHitsApprover proves the form-submit-via-Enter path is
// gated: an OpKey "Enter" on a page whose snapshot names an irreversible action (a "Pay
// now" button) must consult the Approver, and a denial blocks the keypress from reaching
// the session — closing the gate bypass that OpClick-only gating left open.
func TestEnterSubmitOnIrreversiblePageHitsApprover(t *testing.T) {
	fs := &fakeSession{}
	fs.latest = browserwire.Observation{Version: 1, Refs: []browserwire.Ref{
		{ID: 1, Role: "textbox", Name: "Card number", Version: 1},
		{ID: 2, Role: "button", Name: "Pay now", Version: 1},
	}}
	appr := &recordingApprover{verdict: false}
	b := &BrowseTool{Sess: fs, Approver: appr}
	in, _ := json.Marshal(map[string]any{"op": "key", "key": "Enter"})
	out, _, err := b.RunWithImage(context.Background(), ".", in)
	if err != nil {
		t.Fatalf("RunWithImage: %v", err)
	}
	if len(appr.asked) != 1 {
		t.Fatalf("Enter-submit on an irreversible page must consult the gate exactly once, got %d", len(appr.asked))
	}
	if !strings.Contains(out, "BLOCKED") || len(fs.got) != 0 {
		t.Fatalf("a denied Enter-submit must not be dispatched; out=%q sent=%d", out, len(fs.got))
	}
}

// TestEnterKeyOnBenignPageNotGated confirms Enter is ungated when the page carries no
// irreversible signal — so ordinary form navigation (search boxes, logins) is not
// needlessly interrupted.
func TestEnterKeyOnBenignPageNotGated(t *testing.T) {
	fs := &fakeSession{}
	fs.latest = browserwire.Observation{Version: 1, Refs: []browserwire.Ref{
		{ID: 1, Role: "textbox", Name: "Search", Version: 1},
	}}
	appr := &recordingApprover{verdict: false}
	b := &BrowseTool{Sess: fs, Approver: appr}
	in, _ := json.Marshal(map[string]any{"op": "key", "key": "Enter"})
	if _, _, err := b.RunWithImage(context.Background(), ".", in); err != nil {
		t.Fatalf("RunWithImage: %v", err)
	}
	if len(appr.asked) != 0 {
		t.Fatal("Enter on a benign page must not consult the gate")
	}
	if len(fs.got) != 1 {
		t.Fatal("a benign Enter must be dispatched")
	}
}

func TestBenignClickNotGated(t *testing.T) {
	fs := &fakeSession{}
	fs.latest = browserwire.Observation{Version: 1, Refs: []browserwire.Ref{{ID: 1, Role: "link", Name: "Home", Version: 1}}}
	appr := &recordingApprover{verdict: false}
	b := &BrowseTool{Sess: fs, Approver: appr}
	in, _ := json.Marshal(map[string]any{"op": "click", "ref": 1})
	if _, _, err := b.RunWithImage(context.Background(), ".", in); err != nil {
		t.Fatalf("RunWithImage: %v", err)
	}
	if len(appr.asked) != 0 {
		t.Fatal("a benign click must not consult the gate")
	}
	if len(fs.got) != 1 {
		t.Fatal("a benign click must be dispatched")
	}
}

func TestIrreversibleTargetMatching(t *testing.T) {
	latest := browserwire.Observation{Version: 1, Refs: []browserwire.Ref{
		{ID: 1, Role: "button", Name: "Pay now", Version: 1},
		{ID: 2, Role: "button", Name: "Cancel", Version: 1},
		{ID: 3, Role: "button", Name: "Accept all cookies", Version: 1},
	}}
	cases := []struct {
		name string
		act  browserwire.Act
		want bool
	}{
		{"pay-button", browserwire.Act{Op: browserwire.OpClick, Ref: 1}, true},
		{"benign-cancel", browserwire.Act{Op: browserwire.OpClick, Ref: 2}, false},
		{"cookies", browserwire.Act{Op: browserwire.OpClick, Ref: 3}, true},
		{"type-not-gated", browserwire.Act{Op: browserwire.OpType, Ref: 1, Text: "pay now"}, false},
		{"navigate-not-gated", browserwire.Act{Op: browserwire.OpNavigate, URL: "http://pay.test"}, false},
		{"selector-transfer", browserwire.Act{Op: browserwire.OpClick, Selector: "#transfer-funds"}, true},
		// Enter/Return submits the focused form: gated when the page names an irreversible
		// action (the "Pay now" button is in this snapshot), ungated for a benign key.
		{"enter-submit-on-pay-page", browserwire.Act{Op: browserwire.OpKey, Key: "Enter"}, true},
		{"return-submit-on-pay-page", browserwire.Act{Op: browserwire.OpKey, Key: "Return"}, true},
		{"tab-key-not-gated", browserwire.Act{Op: browserwire.OpKey, Key: "Tab"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := irreversibleTarget(tc.act, latest) != ""
			if got != tc.want {
				t.Fatalf("irreversibleTarget(%+v) = %v, want %v", tc.act, got, tc.want)
			}
		})
	}
}
