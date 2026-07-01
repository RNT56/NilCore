package server

import (
	"context"
	"testing"
	"time"

	"nilcore/internal/channel"
	"nilcore/internal/emit"
	"nilcore/internal/policy"
	"nilcore/internal/session"
	"nilcore/internal/wake"
)

// --- minimal fakes (internal package: this test reaches the unexported waker) ---

type stubChannel struct{}

func (stubChannel) Receive(ctx context.Context) (channel.TaskRequest, error) {
	<-ctx.Done()
	return channel.TaskRequest{}, ctx.Err()
}
func (stubChannel) Update(context.Context, string, string) error      { return nil }
func (stubChannel) Ask(context.Context, string, string) (bool, error) { return false, nil }

type stubRouter struct{}

func (stubRouter) Route(context.Context, string, session.WorkState) (session.Route, error) {
	return session.RouteNative, nil
}

// immediateDriver completes a drive at once so a re-engaged Turn settles fast.
type immediateDriver struct{}

func (immediateDriver) Drive(context.Context, session.DriveInput) (session.DriveResult, error) {
	return session.DriveResult{}, nil
}

// memWakeStore is an in-memory wake.Store for the waker test.
type memWakeStore struct{ m map[string]string }

func (s *memWakeStore) SaveWake(_ context.Context, threadID, detail string) error {
	s.m[threadID] = detail
	return nil
}
func (s *memWakeStore) LoadWakes(context.Context) (map[string]string, error) {
	out := make(map[string]string, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out, nil
}
func (s *memWakeStore) DisarmWake(_ context.Context, threadID string) error {
	delete(s.m, threadID)
	return nil
}

// fireDueWakes re-engages only DUE threads, disarms a fired wake (so it never
// re-fires), and skips a not-yet-due one.
func TestFireDueWakesEngagesDueDisarmsAndSkipsFuture(t *testing.T) {
	ctx := context.Background()
	reg := wake.New(&memWakeStore{m: map[string]string{}}, nil)
	if _, err := reg.Arm(ctx, "due", "user-1", 0, "check the CI run"); err != nil { // due now
		t.Fatal(err)
	}
	if _, err := reg.Arm(ctx, "future", "user-1", time.Hour, "later"); err != nil { // not due
		t.Fatal(err)
	}

	srv := &Server{
		Channel: stubChannel{},
		Wake:    reg,
		NewSession: func(_ context.Context, threadID, sender string, _ emit.Emitter, _ policy.Approver) *session.Session {
			s := session.New(threadID, sender, "", nil)
			s.Router = stubRouter{}
			s.Drivers = session.Drivers{Native: immediateDriver{}}
			return s
		},
	}

	srv.fireDueWakes(ctx, time.Now().Add(time.Second)) // now is just past the due wake

	// The due thread was engaged (a Session created for it); the future one was not.
	srv.mu.Lock()
	dueTh, dueOK := srv.threads["due"]
	_, futureOK := srv.threads["future"]
	srv.mu.Unlock()
	if !dueOK {
		t.Error("a DUE wake must engage its thread (no Session created)")
	}
	if futureOK {
		t.Error("a not-yet-due wake must NOT be fired")
	}
	if dueTh != nil {
		dueTh.sess.Wait() // join the re-engaged drive so it doesn't leak past the test
	}

	// The fired wake was disarmed (won't re-fire); the future one is still armed.
	pend, _ := reg.Pending(ctx)
	armed := map[string]bool{}
	for _, w := range pend {
		armed[w.ThreadID] = true
	}
	if armed["due"] {
		t.Error("a fired wake must be disarmed (it would otherwise re-fire forever)")
	}
	if !armed["future"] {
		t.Error("a not-yet-due wake must stay armed")
	}
}

// newWakerTestServer builds a minimal Serve-able server with one DUE wake armed,
// for the SuppressWaker behavior test.
func newWakerTestServer(suppress bool) (*Server, *wake.Registry) {
	reg := wake.New(&memWakeStore{m: map[string]string{}}, nil)
	_, _ = reg.Arm(context.Background(), "due", "user-1", 0, "ping")
	srv := &Server{
		Channel:       stubChannel{}, // blocks until ctx is cancelled
		Wake:          reg,
		SuppressWaker: suppress,
		NewSession: func(_ context.Context, threadID, sender string, _ emit.Emitter, _ policy.Approver) *session.Session {
			s := session.New(threadID, sender, "", nil)
			s.Router = stubRouter{}
			s.Drivers = session.Drivers{Native: immediateDriver{}}
			return s
		},
	}
	return srv, reg
}

// SuppressWaker=true keeps the server from firing wakes (the autonomy daemon owns
// them, through the gated orchestrator); =false makes the server its own waker.
func TestSuppressWakerGatesServerWaker(t *testing.T) {
	old := wakePollInterval
	wakePollInterval = 5 * time.Millisecond
	defer func() { wakePollInterval = old }()

	// Suppressed: a DUE wake must NOT be fired by the server, and must stay armed for
	// the autonomy daemon to pick up. 80ms = 16 poll intervals: ample to catch a
	// (wrongly) running waker, which would fire on its first tick (~5ms).
	srv, reg := newWakerTestServer(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx); close(done) }()
	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done
	srv.mu.Lock()
	_, fired := srv.threads["due"]
	srv.mu.Unlock()
	if fired {
		t.Error("SuppressWaker=true: the server must not fire wakes (autonomy owns them)")
	}
	if pend, _ := reg.Pending(context.Background()); len(pend) != 1 {
		t.Error("a suppressed server must leave the wake armed for the autonomy daemon")
	}

	// Not suppressed: the server's own waker fires the due wake.
	srv2, _ := newWakerTestServer(false)
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { _ = srv2.Serve(ctx2); close(done2) }()
	fired2 := false
	for i := 0; i < 200 && !fired2; i++ {
		srv2.mu.Lock()
		_, fired2 = srv2.threads["due"]
		srv2.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	cancel2()
	<-done2
	if !fired2 {
		t.Error("SuppressWaker=false: the server's own waker must fire due wakes")
	}
}
