package session

import (
	"context"
	"testing"
	"time"

	"nilcore/internal/backend"
	"nilcore/internal/summarize"
)

// A terminal WORK drive fires Notify once with the verifier verdict + branch — the
// "tell the detached principal it's done" push.
func TestNotifyOnTerminalWorkDrive(t *testing.T) {
	drv := newFakeDriver(DriveResult{
		Summary:  summarize.ContextSummary{Goal: "ship it"},
		Branch:   "work-1",
		Verified: true,
	})
	close(drv.release) // let the drive finish immediately
	s := New("chat-local", "local", "/repo", nil)
	s.Router = &fakeRouter{route: RouteNative}
	s.Drivers = Drivers{Native: drv}

	got := make(chan Notification, 1)
	s.Notify = func(n Notification) { got <- n }

	if err := s.Turn(context.Background(), "fix the typo"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	s.Wait()

	select {
	case n := <-got:
		if !n.Verified || n.Failed || n.Branch != "work-1" {
			t.Errorf("notification = %+v, want verified+branch, not failed", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Notify was not called on a terminal work drive")
	}
}

// A plain chat reply is streamed live and must NOT fire a terminal push (it would be
// redundant noise to the thread).
func TestNotifyNotCalledForChat(t *testing.T) {
	drv := newFakeDriver(DriveResult{Summary: summarize.ContextSummary{Goal: "answered"}})
	close(drv.release)
	s := New("chat-local", "local", "/repo", nil)
	s.Router = &fakeRouter{route: RouteChat}
	s.Drivers = Drivers{Chat: drv}

	got := make(chan Notification, 1)
	s.Notify = func(n Notification) { got <- n }

	if err := s.Turn(context.Background(), "what is a mutex?"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	s.Wait()

	select {
	case n := <-got:
		t.Errorf("chat reply must NOT fire a terminal Notify, got %+v", n)
	case <-time.After(200 * time.Millisecond):
		// good: no push for a chat reply
	}
}

// A self-SUSPEND (the agent set a wake timer) returns to Idle without firing Notify —
// the agent is napping, not done; the re-engage happens on wake, not now.
func TestNotifyNotCalledOnSuspend(t *testing.T) {
	drv := newFakeDriver(DriveResult{Summary: summarize.ContextSummary{Goal: "waiting"}})
	drv.err = backend.ErrSuspended
	close(drv.release)
	s := New("chat-local", "local", "/repo", nil)
	s.Router = &fakeRouter{route: RouteNative}
	s.Drivers = Drivers{Native: drv}

	got := make(chan Notification, 1)
	s.Notify = func(n Notification) { got <- n }

	if err := s.Turn(context.Background(), "kick off the long job"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	s.Wait()

	select {
	case n := <-got:
		t.Errorf("a suspended drive must NOT fire a terminal Notify, got %+v", n)
	case <-time.After(200 * time.Millisecond):
		// good: no push on suspend
	}
	if s.PhaseNow() != Idle {
		t.Errorf("a suspended drive must return to Idle, got %v", s.PhaseNow())
	}
}
