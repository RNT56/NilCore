package server_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"nilcore/internal/backend"
	"nilcore/internal/channel"
	"nilcore/internal/emit"
	"nilcore/internal/policy"
	"nilcore/internal/server"
	"nilcore/internal/session"
)

// askingDriver calls the attended ask seam wired onto the drive and reports the
// resolved answer, so the test can prove the channel→session→ask→resolve round-trip.
type askingDriver struct {
	qs      []backend.AskQuestion
	started chan struct{}
	answer  chan string
}

func (d *askingDriver) Drive(ctx context.Context, in session.DriveInput) (session.DriveResult, error) {
	select {
	case d.started <- struct{}{}:
	default:
	}
	if in.AskUser == nil {
		d.answer <- "<no-asker>"
		return session.DriveResult{}, errors.New("ask seam not wired into the serve drive")
	}
	a, err := in.AskUser.Ask(ctx, d.qs)
	if err != nil {
		d.answer <- "<err>"
		return session.DriveResult{}, err
	}
	if len(a) > 0 {
		d.answer <- a[0].Custom
	} else {
		d.answer <- "<empty>"
	}
	return session.DriveResult{Verified: true}, nil
}

// TestServeAskUserRoundTrip: a live serve thread enables ask_user; a drive that asks
// streams the question out as a channel Update and is answered by the operator's next
// authorized thread message (routed intake → Turn → ask box), resuming the drive.
func TestServeAskUserRoundTrip(t *testing.T) {
	fc := &fakeChannel{reqs: make(chan channel.TaskRequest, 4)}
	drv := &askingDriver{
		qs:      []backend.AskQuestion{{Prompt: "which db should I use?"}},
		started: make(chan struct{}, 1),
		answer:  make(chan string, 1),
	}
	srv := &server.Server{
		Channel: fc,
		Auth:    allowlist{"u1": true},
		NewSession: func(_ context.Context, threadID, sender string, out emit.Emitter, _ policy.Approver) *session.Session {
			s := session.New(threadID, sender, "", nil)
			s.Out = out
			s.EnableAskUser(out) // the live-thread attended wiring (mirrors serveSessionFactory)
			s.Router = fakeRouter{}
			s.Drivers = session.Drivers{Native: drv}
			return s
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	// First message starts the drive, which parks on ask_user.
	fc.reqs <- channel.TaskRequest{Goal: "set up the database", ThreadID: "t1", Sender: "u1"}
	select {
	case <-drv.started:
	case <-time.After(2 * time.Second):
		t.Fatal("drive did not start")
	}

	// The question streams out over the channel (proves it was posed AND that the ask
	// box is now collecting, so the answer below cannot race the park).
	waitFor(t, func() bool { return fc.sawUpdateContaining("which db should I use?") },
		"the ask_user question was not streamed to the thread")

	// The operator's reply on the same thread is the ANSWER (intake → Turn → Resolve).
	fc.reqs <- channel.TaskRequest{Goal: "Postgres", ThreadID: "t1", Sender: "u1"}
	select {
	case got := <-drv.answer:
		if got != "Postgres" {
			t.Fatalf("drive received answer %q, want \"Postgres\"", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the thread reply did not resolve the parked ask")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want clean shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not shut down on cancel")
	}
}

// TestSurfaceLineAsk renders an ask question with a marker so it stands out on the
// thread (asserted via the round-trip's update text; this pins the prefix directly).
func TestServeAskQuestionMarker(t *testing.T) {
	fc := &fakeChannel{reqs: make(chan channel.TaskRequest, 2)}
	drv := &askingDriver{
		qs:      []backend.AskQuestion{{Prompt: "unique-marker-probe"}},
		started: make(chan struct{}, 1),
		answer:  make(chan string, 1),
	}
	srv := &server.Server{
		Channel: fc,
		Auth:    allowlist{"u1": true},
		NewSession: func(_ context.Context, threadID, sender string, out emit.Emitter, _ policy.Approver) *session.Session {
			s := session.New(threadID, sender, "", nil)
			s.Out = out
			s.EnableAskUser(out)
			s.Router = fakeRouter{}
			s.Drivers = session.Drivers{Native: drv}
			return s
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	fc.reqs <- channel.TaskRequest{Goal: "go", ThreadID: "t1", Sender: "u1"}
	<-drv.started
	waitFor(t, func() bool { return fc.sawUpdateContaining("❓") && fc.sawUpdateContaining("unique-marker-probe") },
		"question should render with the ❓ marker")
	fc.reqs <- channel.TaskRequest{Goal: "done", ThreadID: "t1", Sender: "u1"}
	<-drv.answer
}
