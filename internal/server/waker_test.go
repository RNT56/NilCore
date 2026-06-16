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
