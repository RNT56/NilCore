package session

import (
	"context"
	"errors"
	"testing"
)

// gatingDriver calls the session-backed gate approver and records the verdict, so a test
// can drive the session into AwaitingGate and resolve it with a typed y/n Turn.
type gatingDriver struct {
	action   string
	started  chan struct{}
	done     chan struct{}
	approved bool
	noGate   bool
}

func (d *gatingDriver) Drive(_ context.Context, in DriveInput) (DriveResult, error) {
	close(d.started)
	if in.Gate == nil {
		d.noGate = true
		close(d.done)
		return DriveResult{}, errors.New("no gate approver wired")
	}
	d.approved = in.Gate.Approve(d.action)
	close(d.done)
	return DriveResult{Verified: true}, nil
}

func newGatingSession(t *testing.T, drv Driver) *Session {
	s := New("chat-local", "local", "/repo", nil)
	s.EnableAskUser(nil) // attended ⇒ launch wires the session gate approver
	s.Router = &fakeRouter{route: RouteNative}
	s.Drivers = Drivers{Native: drv}
	return s
}

// TestGateParkApprove: a gating drive parks AwaitingGate; a typed "y" Turn approves it.
func TestGateParkApprove(t *testing.T) {
	drv := &gatingDriver{action: "push to main", started: make(chan struct{}), done: make(chan struct{})}
	s := newGatingSession(t, drv)
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
	waitPhase(t, s, Idle)
	if drv.noGate {
		t.Fatal("gate approver was not wired into the drive")
	}
	if !drv.approved {
		t.Fatal(`"y" should approve the gate`)
	}
}

// TestGateReprompt: a non-y/n line re-prompts; a following "n" denies.
func TestGateReprompt(t *testing.T) {
	drv := &gatingDriver{action: "deploy", started: make(chan struct{}), done: make(chan struct{})}
	s := newGatingSession(t, drv)
	_ = s.Turn(context.Background(), "go")
	waitClosed(t, drv.started)
	waitFor(t, func() bool { return s.PhaseNow() == AwaitingGate && s.gatePendingNow() })
	if err := s.Turn(context.Background(), "maybe"); err != nil { // not y/n → re-prompt
		t.Fatalf("Turn: %v", err)
	}
	// still parked after the unrecognized answer
	waitFor(t, func() bool { return s.PhaseNow() == AwaitingGate && s.gatePendingNow() })
	_ = s.Turn(context.Background(), "n")
	waitClosed(t, drv.done)
	s.Wait()
	if drv.approved {
		t.Fatal(`"n" should deny the gate`)
	}
}

// TestGateCancelDenies: Cancel while parked unwinds the drive AND the gate denies
// (fail-closed — an abandoned irreversible action is never auto-approved).
func TestGateCancelDenies(t *testing.T) {
	drv := &gatingDriver{action: "rm -rf", started: make(chan struct{}), done: make(chan struct{})}
	s := newGatingSession(t, drv)
	_ = s.Turn(context.Background(), "go")
	waitClosed(t, drv.started)
	waitFor(t, func() bool { return s.PhaseNow() == AwaitingGate && s.gatePendingNow() })
	if !s.Cancel() {
		t.Fatal("Cancel should report a run was cancelled")
	}
	waitClosed(t, drv.done)
	waitPhase(t, s, Idle)
	if drv.approved {
		t.Fatal("a cancelled gate must DENY (fail-closed)")
	}
}

// gatePendingNow exposes gatePending under the lock for tests.
func (s *Session) gatePendingNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gatePending
}

// TestParseYesNo pins the gate answer grammar.
func TestParseYesNo(t *testing.T) {
	cases := []struct {
		in        string
		ans, recd bool
	}{
		{"y", true, true}, {"Y", true, true}, {"yes", true, true}, {" Yes ", true, true},
		{"n", false, true}, {"no", false, true}, {"N", false, true},
		{"maybe", false, false}, {"", false, false}, {"sure", false, false},
	}
	for _, c := range cases {
		a, r := parseYesNo(c.in)
		if a != c.ans || r != c.recd {
			t.Errorf("parseYesNo(%q) = (%v,%v), want (%v,%v)", c.in, a, r, c.ans, c.recd)
		}
	}
}
