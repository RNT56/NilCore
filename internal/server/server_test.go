package server_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"nilcore/internal/channel"
	"nilcore/internal/emit"
	"nilcore/internal/policy"
	"nilcore/internal/server"
	"nilcore/internal/session"
)

// fakeChannel is a hermetic in-memory Channel: Receive yields scripted requests,
// Update records every progress line, Ask answers a fixed verdict. It never touches
// the network.
type fakeChannel struct {
	reqs chan channel.TaskRequest

	mu      sync.Mutex
	updates []string
}

func (f *fakeChannel) Receive(ctx context.Context) (channel.TaskRequest, error) {
	select {
	case r, ok := <-f.reqs:
		if !ok {
			<-ctx.Done()
			return channel.TaskRequest{}, ctx.Err()
		}
		return r, nil
	case <-ctx.Done():
		return channel.TaskRequest{}, ctx.Err()
	}
}

func (f *fakeChannel) Update(_ context.Context, _ string, msg string) error {
	f.mu.Lock()
	f.updates = append(f.updates, msg)
	f.mu.Unlock()
	return nil
}

func (f *fakeChannel) Ask(context.Context, string, string) (bool, error) { return true, nil }

func (f *fakeChannel) sawUpdateContaining(want string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.updates {
		if strings.Contains(u, want) {
			return true
		}
	}
	return false
}

// allowlist is a trivial Authorizer: Permit returns true only for listed senders.
type allowlist map[string]bool

func (a allowlist) Permit(p string) bool { return a[p] }

// fakeRouter always routes RouteNative (the test exercises the native arm; the
// route choice is not under test here).
type fakeRouter struct{}

func (fakeRouter) Route(context.Context, string, session.WorkState) (session.Route, error) {
	return session.RouteNative, nil
}

// blockingDriver is a fake native Driver that holds the drive "Working" until
// released, draining the session Inbox so the test can observe queued user turns,
// and reporting when a steer signal arrived. It lets the test prove first-message-
// starts-a-drive, second-message-queues, and !-message-steers without a model run.
type blockingDriver struct {
	started chan struct{} // a drive began
	release chan struct{} // the test closes this to let the drive finish
	steered chan struct{} // signalled when the Inbox steer channel fired

	mu       sync.Mutex
	drainedN int // number of queued user turns drained at the boundary
	calls    int
}

func (d *blockingDriver) Drive(ctx context.Context, in session.DriveInput) (session.DriveResult, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()

	select {
	case d.started <- struct{}{}:
	default:
	}

	// Watch the steer signal concurrently while we wait for release, so a '!'-marked
	// follow-up is observed (the loop's per-iteration steer watcher, simplified).
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		for {
			select {
			case <-in.Inbox.Steer():
				select {
				case d.steered <- struct{}{}:
				default:
				}
			case <-d.release:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	select {
	case <-d.release:
	case <-ctx.Done():
	}
	<-watchDone

	// Drain at the (single, terminal) boundary: count the folded follow-ups.
	d.mu.Lock()
	d.drainedN = len(in.Inbox.Drain())
	d.mu.Unlock()

	return session.DriveResult{Verified: true}, nil
}

func (d *blockingDriver) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *blockingDriver) drained() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.drainedN
}

// factoryRec records what the SessionFactory built so a test can assert per-thread
// isolation and that the server handed each Session a (channel-bound) Emitter.
type factoryRec struct {
	mu       sync.Mutex
	sessions map[string]*session.Session
	outs     []emit.Emitter
}

func (r *factoryRec) sessionCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

func (r *factoryRec) session(id string) *session.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[id]
}

// newServer wires a Server whose factory builds a real session.Session driven by
// the supplied Driver, so the test exercises the genuine Turn/Inbox path.
func newServer(fc *fakeChannel, auth server.Authorizer, drv session.Driver) (*server.Server, *factoryRec) {
	rec := &factoryRec{sessions: map[string]*session.Session{}}
	srv := &server.Server{
		Channel: fc,
		Auth:    auth,
		NewSession: func(_ context.Context, threadID, sender string, out emit.Emitter, _ policy.Approver) *session.Session {
			s := session.New(threadID, sender, "", nil)
			s.Out = out
			s.Router = fakeRouter{}
			s.Drivers = session.Drivers{Native: drv}
			rec.mu.Lock()
			rec.sessions[threadID] = s
			rec.outs = append(rec.outs, out)
			rec.mu.Unlock()
			return s
		},
	}
	return srv, rec
}

// TestFirstMessageStartsDriveQueueAndSteer is the core acceptance: a first message
// starts a drive; a second plain message mid-drive queues; a '!'-message steers; and
// both follow-ups are folded as user turns drained at the boundary.
func TestFirstMessageStartsDriveQueueAndSteer(t *testing.T) {
	fc := &fakeChannel{reqs: make(chan channel.TaskRequest, 4)}
	drv := &blockingDriver{started: make(chan struct{}, 1), release: make(chan struct{}), steered: make(chan struct{}, 1)}
	srv, _ := newServer(fc, allowlist{"u1": true}, drv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	// First message → starts a drive.
	fc.reqs <- channel.TaskRequest{Goal: "do the thing", ThreadID: "t1", Sender: "u1"}
	select {
	case <-drv.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first message did not start a drive")
	}

	// Second message mid-drive (plain) → QUEUE.
	fc.reqs <- channel.TaskRequest{Goal: "also handle errors", ThreadID: "t1", Sender: "u1"}

	// Third message mid-drive ('!') → STEER (fires the steer signal).
	fc.reqs <- channel.TaskRequest{Goal: "!stop, wrong path", ThreadID: "t1", Sender: "u1"}
	select {
	case <-drv.steered:
	case <-time.After(2 * time.Second):
		t.Fatal("'!'-marked follow-up did not steer the in-flight drive")
	}

	// Let the drive finish and drain its boundary.
	close(drv.release)

	// The drive should have drained exactly the two folded follow-ups (queue + steer).
	waitFor(t, func() bool { return drv.drained() == 2 }, "expected 2 folded follow-ups at the boundary")

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

// TestServeControlVerbPinsMode proves serve-channel parity: a "/plan" over the
// channel pins the thread's mode and is NOT run as a task (no drive started, no
// "Starting:" announced) — it is a control, acked via Channel.Update.
func TestServeControlVerbPinsMode(t *testing.T) {
	fc := &fakeChannel{reqs: make(chan channel.TaskRequest, 2)}
	drv := &blockingDriver{started: make(chan struct{}, 1), release: make(chan struct{}), steered: make(chan struct{}, 1)}
	srv, rec := newServer(fc, allowlist{"u1": true}, drv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	fc.reqs <- channel.TaskRequest{Goal: "/plan", ThreadID: "t1", Sender: "u1"}
	waitFor(t, func() bool { return fc.sawUpdateContaining("mode → plan") }, "no mode ack over the channel")

	// The thread's Session is pinned to plan, NO drive started, and the control-only
	// first message was not announced as a task.
	if s := rec.session("t1"); s == nil || s.CurrentMode() != session.ModePlan {
		t.Fatalf("thread mode not pinned to plan via /plan over the channel")
	}
	select {
	case <-drv.started:
		t.Fatal("a control verb must not start a drive")
	case <-time.After(100 * time.Millisecond):
	}
	if fc.sawUpdateContaining("Starting:") {
		t.Error("a control-only first message must not announce 'Starting:'")
	}

	cancel()
	<-done
}

// TestUnauthorizedRefusedBeforeTurn proves the trust line: an unauthorized sender's
// message is refused (logged + told) and NEVER reaches Turn — the driver is never
// invoked, and no Session is created for the unauthorized sender's thread.
func TestUnauthorizedRefusedBeforeTurn(t *testing.T) {
	fc := &fakeChannel{reqs: make(chan channel.TaskRequest, 2)}
	drv := &blockingDriver{started: make(chan struct{}, 1), release: make(chan struct{}), steered: make(chan struct{}, 1)}
	srv, rec := newServer(fc, allowlist{"good": true}, drv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	// Unauthorized sender — including a steer-marked message — must be refused.
	fc.reqs <- channel.TaskRequest{Goal: "!do evil", ThreadID: "tx", Sender: "intruder"}

	waitFor(t, func() bool { return fc.sawUpdateContaining("Unauthorized") }, "unauthorized sender was not told")

	if drv.callCount() != 0 {
		t.Fatalf("driver ran for an unauthorized sender (calls=%d) — trust line breached", drv.callCount())
	}
	if rec.sessionCount() != 0 {
		t.Fatalf("a Session was created for an unauthorized sender (count=%d)", rec.sessionCount())
	}

	// A subsequent authorized message still works — refusal did not wedge the server.
	close(drv.release)
	fc.reqs <- channel.TaskRequest{Goal: "do good", ThreadID: "tg", Sender: "good"}
	select {
	case <-drv.started:
	case <-time.After(2 * time.Second):
		t.Fatal("authorized message after a refusal did not start a drive")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not shut down")
	}
}

// TestSenderPinRefusesForeignSender proves a thread is one principal's conversation:
// a second, still-authorized but DIFFERENT sender to the same thread is refused.
func TestSenderPinRefusesForeignSender(t *testing.T) {
	fc := &fakeChannel{reqs: make(chan channel.TaskRequest, 3)}
	drv := &blockingDriver{started: make(chan struct{}, 1), release: make(chan struct{}), steered: make(chan struct{}, 1)}
	srv, rec := newServer(fc, allowlist{"a": true, "b": true}, drv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	fc.reqs <- channel.TaskRequest{Goal: "mine", ThreadID: "shared", Sender: "a"}
	select {
	case <-drv.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first message did not start a drive")
	}

	// 'b' is authorized to command the agent, but not to drive 'a's thread.
	fc.reqs <- channel.TaskRequest{Goal: "intrude", ThreadID: "shared", Sender: "b"}
	waitFor(t, func() bool { return fc.sawUpdateContaining("owned by another principal") },
		"foreign sender was not refused on a pinned thread")

	if rec.sessionCount() != 1 {
		t.Fatalf("foreign sender created a second Session (count=%d), want the single pinned one", rec.sessionCount())
	}

	close(drv.release)
	cancel()
	<-done
}

// TestEmitterRoutesToUpdate proves the Session's Emitter (handed in by the server)
// routes reasoning to Channel.Update on the right thread.
func TestEmitterRoutesToUpdate(t *testing.T) {
	fc := &fakeChannel{reqs: make(chan channel.TaskRequest, 1)}

	emitted := make(chan struct{})
	emitting := driverFunc(func(_ context.Context, in session.DriveInput) (session.DriveResult, error) {
		if in.Out != nil {
			in.Out.Emit(emit.Event{Kind: emit.KindIntent, Text: "about to do X"})
		}
		close(emitted)
		return session.DriveResult{Verified: true}, nil
	})

	srv, _ := newServer(fc, allowlist{"u1": true}, emitting)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	fc.reqs <- channel.TaskRequest{Goal: "go", ThreadID: "t1", Sender: "u1"}
	select {
	case <-emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("driver did not emit")
	}

	waitFor(t, func() bool { return fc.sawUpdateContaining("about to do X") },
		"emitted reasoning did not reach Channel.Update")

	cancel()
	<-done
}

// TestTwoThreadsIndependentSessions proves the per-thread map: two threads get two
// independent Sessions, each able to drive concurrently.
func TestTwoThreadsIndependentSessions(t *testing.T) {
	fc := &fakeChannel{reqs: make(chan channel.TaskRequest, 4)}
	started := make(chan string, 4)
	rel := make(chan struct{})
	drv := driverFunc(func(ctx context.Context, in session.DriveInput) (session.DriveResult, error) {
		started <- in.Goal
		<-rel
		return session.DriveResult{Verified: true}, nil
	})
	srv, rec := newServer(fc, allowlist{"u1": true, "u2": true}, drv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	fc.reqs <- channel.TaskRequest{Goal: "thread one", ThreadID: "t1", Sender: "u1"}
	fc.reqs <- channel.TaskRequest{Goal: "thread two", ThreadID: "t2", Sender: "u2"}

	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case g := <-started:
			got[g] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 2 threads started concurrently", len(got))
		}
	}
	if !got["thread one"] || !got["thread two"] {
		t.Fatalf("both threads should drive concurrently, got %v", got)
	}
	if rec.sessionCount() != 2 {
		t.Fatalf("expected 2 independent Sessions, got %d", rec.sessionCount())
	}

	close(rel)
	cancel()
	<-done
}

// TestServeRequiresFactory proves a misconfigured Server (no factory) fails fast
// rather than nil-panicking on the first message.
func TestServeRequiresFactory(t *testing.T) {
	fc := &fakeChannel{reqs: make(chan channel.TaskRequest)}
	srv := &server.Server{Channel: fc, Auth: allowlist{"u": true}}
	if err := srv.Serve(context.Background()); err == nil {
		t.Fatal("Serve with no NewSession factory should error")
	}
}

// driverFunc adapts a func to session.Driver for the lightweight tests.
type driverFunc func(context.Context, session.DriveInput) (session.DriveResult, error)

func (f driverFunc) Drive(ctx context.Context, in session.DriveInput) (session.DriveResult, error) {
	return f(ctx, in)
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal(msg)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
