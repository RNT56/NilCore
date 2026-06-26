package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"nilcore/internal/backend"
)

// askingDriver calls the attended ask seam and records the answer it received, so a
// test can drive the session into AwaitingInput and back.
type askingDriver struct {
	qs      []backend.AskQuestion
	started chan struct{}
	done    chan struct{}
	ans     []backend.AskAnswer
	err     error
}

func (d *askingDriver) Drive(ctx context.Context, in DriveInput) (DriveResult, error) {
	close(d.started)
	if in.AskUser == nil {
		d.err = errors.New("AskUser not wired into the drive")
		close(d.done)
		return DriveResult{}, d.err
	}
	d.ans, d.err = in.AskUser.Ask(ctx, d.qs)
	close(d.done)
	if d.err != nil {
		return DriveResult{}, d.err
	}
	return DriveResult{Verified: true}, nil
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// TestAskParkResolveResume: a drive that asks parks the session in AwaitingInput; the
// NEXT Turn is routed as the ANSWER (not queued), and the drive resumes and finishes.
func TestAskParkResolveResume(t *testing.T) {
	drv := &askingDriver{
		qs:      []backend.AskQuestion{{Prompt: "which db?"}},
		started: make(chan struct{}),
		done:    make(chan struct{}),
	}
	s := New("chat-local", "local", "/repo", nil)
	s.EnableAskUser(nil)
	s.Router = &fakeRouter{route: RouteNative}
	s.Drivers = Drivers{Native: drv}

	if err := s.Turn(context.Background(), "go"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	waitClosed(t, drv.started)
	waitFor(t, func() bool { return s.PhaseNow() == AwaitingInput && s.askBox.Pending() })

	// The follow-up while parked is the ANSWER — routed to the ask box, not the inbox.
	if err := s.Turn(context.Background(), "Postgres"); err != nil {
		t.Fatalf("answer Turn: %v", err)
	}
	waitClosed(t, drv.done)
	s.Wait()
	waitPhase(t, s, Idle)

	if drv.err != nil {
		t.Fatalf("driver err: %v", drv.err)
	}
	if len(drv.ans) != 1 || drv.ans[0].Custom != "Postgres" {
		t.Fatalf("answer = %+v, want one free-form 'Postgres'", drv.ans)
	}
}

// TestAskParkCancel: /cancel (Session.Cancel) while parked unwinds the drive cleanly
// to Idle; the parked Ask returns a cancellation.
func TestAskParkCancel(t *testing.T) {
	drv := &askingDriver{
		qs:      []backend.AskQuestion{{Prompt: "q"}},
		started: make(chan struct{}),
		done:    make(chan struct{}),
	}
	s := New("chat-local", "local", "/repo", nil)
	s.EnableAskUser(nil)
	s.Router = &fakeRouter{route: RouteNative}
	s.Drivers = Drivers{Native: drv}

	if err := s.Turn(context.Background(), "go"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	waitClosed(t, drv.started)
	waitFor(t, func() bool { return s.PhaseNow() == AwaitingInput && s.askBox.Pending() })

	if !s.Cancel() {
		t.Fatal("Cancel should report a run was cancelled")
	}
	waitClosed(t, drv.done)
	waitPhase(t, s, Idle)
	if !errors.Is(drv.err, context.Canceled) {
		t.Fatalf("parked Ask err = %v, want context.Canceled", drv.err)
	}
}

// TestAskLevelScale exercises the level→budget mapping, the dial, and round-trip.
func TestAskLevelScale(t *testing.T) {
	s := New("c", "local", "/repo", nil)
	s.EnableAskUser(nil)
	if got := s.AskLevelName(); got != "normal" {
		t.Fatalf("default level = %q, want normal", got)
	}
	if got := s.askMaxAsks(); got != 3 {
		t.Fatalf("default budget = %d, want 3", got)
	}
	if ack, err := s.SetAskLevelSpec("less"); err != nil || s.AskLevelName() != "low" {
		t.Fatalf("less → %q (%v), ack=%q", s.AskLevelName(), err, ack)
	}
	if got := s.askMaxAsks(); got != 2 {
		t.Fatalf("low budget = %d, want 2", got)
	}
	if _, err := s.SetAskLevelSpec("off"); err != nil || s.askMaxAsks() != 0 {
		t.Fatalf("off → budget %d (%v)", s.askMaxAsks(), err)
	}
	if _, err := s.SetAskLevelSpec("more"); err != nil || s.AskLevelName() != "minimal" {
		t.Fatalf("more from off → %q (%v)", s.AskLevelName(), err)
	}
	if _, err := s.SetAskLevelSpec("bogus"); err == nil {
		t.Fatal("an unknown spec should error")
	}

	// Posture round-trips persistence (like Mode).
	s.State.AskLevel = askLevelHigh
	if dec := encodeState(s.State).decode(); dec.AskLevel != askLevelHigh {
		t.Fatalf("AskLevel did not round-trip: got %d", dec.AskLevel)
	}
	// A zero (pre-feature snapshot) normalizes to the default, never silently off.
	if got := normalizeAskLevel(0); got != askLevelNormal {
		t.Fatalf("zero level normalized to %d, want normal", got)
	}
	if askBudgetFor(0) == 0 {
		t.Fatal("zero (unset) level must not map to off")
	}
}
