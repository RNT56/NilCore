package session

// Tests for the session gate's structured-evidence path (gate.go): the session
// approver opts in to policy.StructuredApprover, mirrors the GateAction's
// evidence into an emit.GatePrompt on the SAME KindGate event, and keeps the
// legacy flat path byte-identical (Gate stays nil).

import (
	"context"
	"errors"
	"sync"
	"testing"

	"nilcore/internal/emit"
	"nilcore/internal/policy"
)

// captureEmitter records every event so the test can inspect the KindGate payload.
type captureEmitter struct {
	mu     sync.Mutex
	events []emit.Event
}

func (c *captureEmitter) Emit(ev emit.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *captureEmitter) gates() []emit.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []emit.Event
	for _, ev := range c.events {
		if ev.Kind == emit.KindGate {
			out = append(out, ev)
		}
	}
	return out
}

// structuredGatingDriver routes an evidence-carrying GateAction through the
// session approver's STRUCTURED path (the wired approver must implement
// policy.StructuredApprover for the payload to survive).
type structuredGatingDriver struct {
	action   policy.GateAction
	started  chan struct{}
	done     chan struct{}
	approved bool
	flat     bool // the approver did NOT implement StructuredApprover
}

func (d *structuredGatingDriver) Drive(_ context.Context, in DriveInput) (DriveResult, error) {
	close(d.started)
	if in.Gate == nil {
		close(d.done)
		return DriveResult{}, errors.New("no gate approver wired")
	}
	sa, ok := in.Gate.(policy.StructuredApprover)
	if !ok {
		d.flat = true
		close(d.done)
		return DriveResult{}, errors.New("session approver lost StructuredApprover")
	}
	d.approved = sa.ApproveStructured(d.action)
	close(d.done)
	return DriveResult{Verified: true}, nil
}

// TestGateStructuredEvidenceEmitted: the structured path emits ONE KindGate event
// whose Text is the flattened Describe() line (unchanged surface fallback) and
// whose Gate payload mirrors the action's evidence field-for-field.
func TestGateStructuredEvidenceEmitted(t *testing.T) {
	action := policy.GateAction{
		Type: policy.PromoteToBase, Branch: "main", Detail: "verified tip",
		Evidence: &policy.GateEvidence{
			Diffstat:    "2 file(s) changed, +9 −1",
			DiffExcerpt: "diff --git a/x b/x\n+added",
			VerifyTail:  "all checks passed",
			SpentUSD:    0.75,
		},
	}
	drv := &structuredGatingDriver{action: action, started: make(chan struct{}), done: make(chan struct{})}
	s := newGatingSession(t, drv)
	sink := &captureEmitter{}
	s.Out = sink

	if err := s.Turn(context.Background(), "go"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	waitClosed(t, drv.started)
	waitFor(t, func() bool { return s.PhaseNow() == AwaitingGate && s.gatePendingNow() })
	if err := s.Turn(context.Background(), "y"); err != nil {
		t.Fatalf("answer Turn: %v", err)
	}
	waitClosed(t, drv.done)
	s.Wait()

	if drv.flat {
		t.Fatal("the session gate approver must implement policy.StructuredApprover")
	}
	if !drv.approved {
		t.Fatal(`"y" should approve the structured gate`)
	}
	gates := sink.gates()
	if len(gates) != 1 {
		t.Fatalf("want 1 KindGate event, got %d", len(gates))
	}
	ev := gates[0]
	if ev.Text != action.Describe() {
		t.Errorf("Text = %q, want the flattened Describe() %q", ev.Text, action.Describe())
	}
	gp := ev.Gate
	if gp == nil {
		t.Fatal("KindGate event must carry the mirrored GatePrompt")
	}
	want := action.Evidence
	if gp.Action != action.Describe() || gp.Diffstat != want.Diffstat ||
		gp.DiffExcerpt != want.DiffExcerpt || gp.VerifyTail != want.VerifyTail ||
		gp.SpentUSD != want.SpentUSD {
		t.Errorf("mirror drifted:\n got %+v\nwant %+v (Action=%q)", gp, want, action.Describe())
	}
}

// TestGateStructuredWithoutEvidenceStaysFlat: an evidence-less structured action —
// and the legacy Approve path (pinned by gate_test.go) — emit a KindGate event with
// a nil Gate payload, so payload-ignoring surfaces render byte-identically.
func TestGateStructuredWithoutEvidenceStaysFlat(t *testing.T) {
	action := policy.GateAction{Type: policy.Push, Branch: "main"}
	drv := &structuredGatingDriver{action: action, started: make(chan struct{}), done: make(chan struct{})}
	s := newGatingSession(t, drv)
	sink := &captureEmitter{}
	s.Out = sink

	_ = s.Turn(context.Background(), "go")
	waitClosed(t, drv.started)
	waitFor(t, func() bool { return s.PhaseNow() == AwaitingGate && s.gatePendingNow() })
	_ = s.Turn(context.Background(), "n")
	waitClosed(t, drv.done)
	s.Wait()

	if drv.approved {
		t.Fatal(`"n" should deny`)
	}
	gates := sink.gates()
	if len(gates) != 1 {
		t.Fatalf("want 1 KindGate event, got %d", len(gates))
	}
	if gates[0].Gate != nil {
		t.Errorf("evidence-less gate must emit a nil payload, got %+v", gates[0].Gate)
	}
	if gates[0].Text != action.Describe() {
		t.Errorf("Text = %q, want %q", gates[0].Text, action.Describe())
	}
}
