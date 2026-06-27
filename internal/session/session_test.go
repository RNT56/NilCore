package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nilcore/internal/inbox"
	"nilcore/internal/model"
	"nilcore/internal/summarize"
)

// fakeRouter returns a fixed route (or error), recording the text/state it saw.
type fakeRouter struct {
	route   Route
	err     error
	mu      sync.Mutex
	calls   int
	lastTxt string
	lastSt  WorkState
}

func (f *fakeRouter) Route(_ context.Context, text string, st WorkState) (Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastTxt = text
	f.lastSt = st
	return f.route, f.err
}

// fakeDriver blocks on a release channel so a test can observe the Working phase
// while the drive is "running", then returns a fixed result. It records the
// DriveInput it received (to assert History/Inbox/Out are threaded through).
type fakeDriver struct {
	release chan struct{} // closed/closeable to let Drive return
	started chan struct{} // closed once Drive has begun (and recorded its input)
	result  DriveResult
	err     error
	drained int32 // count of Inbox messages drained inside the drive

	mu sync.Mutex
	in DriveInput
}

func newFakeDriver(res DriveResult) *fakeDriver {
	return &fakeDriver{
		release: make(chan struct{}),
		started: make(chan struct{}),
		result:  res,
	}
}

func (f *fakeDriver) Drive(ctx context.Context, in DriveInput) (DriveResult, error) {
	f.mu.Lock()
	f.in = in
	f.mu.Unlock()
	close(f.started)

	// Wait until released, draining any mid-work messages once so the test can
	// confirm a queued/steered follow-up reaches the running loop's Inbox.
	select {
	case <-f.release:
	case <-ctx.Done():
		return DriveResult{}, ctx.Err()
	}
	if in.Inbox != nil {
		atomic.AddInt32(&f.drained, int32(len(in.Inbox.Drain())))
	}
	return f.result, f.err
}

func (f *fakeDriver) input() DriveInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.in
}

// testWaitBudget bounds the package's test-sync helpers (waitClosed, waitFor). It is
// deliberately generous: the helpers return the instant their condition holds, so the
// bound only ever bites a genuine hang — a tighter value flaked under CI load.
const testWaitBudget = 10 * time.Second

// waitClosed blocks until ch closes or the deadline elapses (test sync without
// arbitrary sleeps). The bound is generous (returns the instant ch closes, so it only
// bites a genuine hang) to tolerate scheduling jitter on a loaded CI runner — a 2s bound
// flaked there even though the channel closes in milliseconds locally.
func waitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(testWaitBudget):
		t.Fatal("timed out waiting for channel close")
	}
}

// waitPhase polls PhaseNow until it equals want or the deadline elapses.
func waitPhase(t *testing.T, s *Session, want Phase) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.PhaseNow() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("phase never reached %v (now %v)", want, s.PhaseNow())
}

func text(msgs []model.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		var s string
		for _, b := range m.Content {
			s += b.Text
		}
		out = append(out, s)
	}
	return out
}

// --- classifyInterrupt: the local queue-vs-steer rule (no LLM) -----------------

func TestClassifyInterrupt(t *testing.T) {
	cases := []struct {
		name string
		text string
		want inbox.Mode
	}{
		{"plain queues", "add a rate limiter", inbox.Queue},
		{"bang steers", "!the path is wrong", inbox.Steer},
		{"bang with leading space steers", "   !fix it", inbox.Steer},
		{"slash steer command steers", "/steer use ./service", inbox.Steer},
		{"slash steer bare steers", "/steer", inbox.Steer},
		{"slash other queues", "/status", inbox.Queue},
		{"empty queues", "", inbox.Queue},
		{"bang mid-text queues", "wait! stop", inbox.Queue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyInterrupt(tc.text); got != tc.want {
				t.Fatalf("classifyInterrupt(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// --- Idle Turn: routes + launches a drive --------------------------------------

func TestTurnWhileIdleRoutesAndLaunches(t *testing.T) {
	rt := &fakeRouter{route: RouteNative}
	drv := newFakeDriver(DriveResult{
		Summary:  summarize.ContextSummary{Goal: "ship it", Remaining: "tests"},
		Branch:   "work-1",
		Outcome:  "compiles",
		Verified: true,
	})
	s := New("chat-local", "local", "/repo", nil)
	s.Router = rt
	s.Drivers = Drivers{Native: drv}

	if err := s.Turn(context.Background(), "fix the typo"); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	// The router saw the message; the driver started in its goroutine.
	waitClosed(t, drv.started)
	if rt.calls != 1 {
		t.Fatalf("router calls = %d, want 1", rt.calls)
	}
	if rt.lastTxt != "fix the typo" {
		t.Fatalf("router text = %q", rt.lastTxt)
	}
	waitPhase(t, s, Working)

	// The drive received the History (continue-not-restart) and the live Inbox.
	in := drv.input()
	if got := text(in.History); len(got) != 1 || got[0] != "fix the typo" {
		t.Fatalf("drive History = %v, want [fix the typo]", got)
	}
	if in.Inbox == nil {
		t.Fatal("drive Inbox not wired")
	}
	if in.Route != RouteNative {
		t.Fatalf("drive Route = %v, want native", in.Route)
	}

	// Let the drive finish; Phase returns to Idle and State is folded.
	close(drv.release)
	s.Wait()
	waitPhase(t, s, Idle)

	if got := s.State.Summary.Goal; got != "ship it" {
		t.Fatalf("folded Summary.Goal = %q, want ship it", got)
	}
	if s.State.Branch != "work-1" {
		t.Fatalf("folded Branch = %q", s.State.Branch)
	}
	if s.State.LastOutcome != "compiles" {
		t.Fatalf("folded LastOutcome = %q", s.State.LastOutcome)
	}
	if s.State.Active != RouteNative {
		t.Fatalf("State.Active = %v, want native", s.State.Active)
	}
}

// --- Mode override: a pinned mode bypasses the router --------------------------

func TestRouteForMode(t *testing.T) {
	s := New("c", "local", "/repo", nil)

	// Discuss/Plan always run the single native loop (read-only capability is set
	// from the mode in the DriveInput, not by choosing a different machine).
	for _, m := range []Mode{ModeDiscuss, ModePlan} {
		if r, ok := s.routeForMode(m, "anything"); !ok || r != RouteNative {
			t.Errorf("routeForMode(%v) = (%v,%v), want (native,true)", m, r, ok)
		}
	}

	// Execute sizes native-vs-supervise via the injected Sizer.
	s.Sizer = func(goal string) bool { return strings.Contains(goal, "big") }
	if r, ok := s.routeForMode(ModeExecute, "small fix"); !ok || r != RouteNative {
		t.Errorf("execute(small) = (%v,%v), want (native,true)", r, ok)
	}
	if r, ok := s.routeForMode(ModeExecute, "big multi-file feature"); !ok || r != RouteSupervise {
		t.Errorf("execute(big) = (%v,%v), want (supervise,true)", r, ok)
	}

	// Auto falls through to the classifier (ok=false).
	if _, ok := s.routeForMode(ModeAuto, "x"); ok {
		t.Errorf("auto must not override the router (ok=true)")
	}
}

// A pinned read-only mode must (a) NOT call the auto-router at all, and (b) thread
// the mode into the DriveInput so the wiring closure builds a read-only backend.
// This is the behavioral half of "user sets a mode, agent obeys it."
func TestPinnedModeBypassesRouterAndThreadsMode(t *testing.T) {
	rt := &fakeRouter{route: RouteSupervise} // would route elsewhere if ever consulted
	drv := newFakeDriver(DriveResult{Verified: true})
	s := New("chat-local", "local", "/repo", nil)
	s.Router = rt
	s.Drivers = Drivers{Native: drv}
	s.SetMode(ModePlan)

	if err := s.Turn(context.Background(), "how should I add rate limiting?"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	waitClosed(t, drv.started)
	waitPhase(t, s, Working)

	if rt.calls != 0 {
		t.Fatalf("router was consulted %d times; a pinned mode must bypass it", rt.calls)
	}
	in := drv.input()
	if in.Route != RouteNative {
		t.Fatalf("drive Route = %v, want native (plan runs the read-only native loop)", in.Route)
	}
	if in.Mode != ModePlan {
		t.Fatalf("drive Mode = %v, want plan (capability must follow the pinned mode)", in.Mode)
	}

	close(drv.release)
	s.Wait()
}

// An added read root must be captured into the launched DriveInput (so the wiring
// closure can build the read/search tools with it) and must NOT be duplicated.
func TestAddReadRootThreadsIntoDrive(t *testing.T) {
	rt := &fakeRouter{route: RouteNative}
	drv := newFakeDriver(DriveResult{Verified: true})
	s := New("c", "local", "/repo", nil)
	s.Router = rt
	s.Drivers = Drivers{Native: drv}
	s.AddReadRoot("/abs/lib")
	s.AddReadRoot("/abs/lib") // duplicate ignored
	s.AddReadRoot("/abs/docs")

	if err := s.Turn(context.Background(), "use the lib"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	waitClosed(t, drv.started)
	waitPhase(t, s, Working)

	in := drv.input()
	if len(in.ReadRoots) != 2 || in.ReadRoots[0] != "/abs/lib" || in.ReadRoots[1] != "/abs/docs" {
		t.Fatalf("drive ReadRoots = %v, want [/abs/lib /abs/docs] (deduped)", in.ReadRoots)
	}

	close(drv.release)
	s.Wait()
}

// --- Cancel: abort the in-flight run, stay in the conversation ------------------

func TestSessionCancelAbortsInFlightDrive(t *testing.T) {
	rt := &fakeRouter{route: RouteNative}
	drv := newFakeDriver(DriveResult{Summary: summarize.ContextSummary{Goal: "x"}})
	s := New("chat-local", "local", "/repo", nil)
	s.Router = rt
	s.Drivers = Drivers{Native: drv}

	if err := s.Turn(context.Background(), "build the whole thing"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	waitClosed(t, drv.started)
	waitPhase(t, s, Working)

	// Cancel aborts the in-flight drive and returns only after it unwinds to Idle:
	// the drive's ctx is cancelled (the fakeDriver returns ctx.Err()), so a new run
	// can start immediately. Distinct from queue/steer, which fold a turn instead.
	if !s.Cancel() {
		t.Fatal("Cancel should report it aborted an in-flight run")
	}
	if p := s.PhaseNow(); p != Idle {
		t.Fatalf("after Cancel the session must be Idle, got %v", p)
	}
	// A cancelled drive's result is NOT folded (Drive returned an error), so State
	// stays empty — the conversation is ready for a fresh instruction.
	if s.State.Summary.Goal != "" {
		t.Errorf("a cancelled drive must not fold State, got Summary.Goal=%q", s.State.Summary.Goal)
	}
	// Cancel on an Idle session is a no-op.
	if s.Cancel() {
		t.Error("Cancel on an Idle session should return false (nothing to cancel)")
	}
}

// --- Working Turn: pushes to the inbox (queue vs steer per the prefix) ----------

func TestTurnWhileWorkingPushesToInbox(t *testing.T) {
	cases := []struct {
		name     string
		followup string
		wantMode inbox.Mode
	}{
		{"queue default", "also add logging", inbox.Queue},
		{"steer prefix", "!use ./service", inbox.Steer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeRouter{route: RouteNative}
			drv := newFakeDriver(DriveResult{Verified: true})
			s := New("chat-local", "local", "/repo", nil)
			s.Router = rt
			s.Drivers = Drivers{Native: drv}

			if err := s.Turn(context.Background(), "build it"); err != nil {
				t.Fatalf("Turn(idle): %v", err)
			}
			waitClosed(t, drv.started)
			waitPhase(t, s, Working)

			// Mid-work follow-up: Turn returns immediately and pushes to the Inbox.
			steered := make(chan struct{})
			go func() {
				select {
				case <-s.Inbox.Steer():
					close(steered)
				case <-time.After(time.Second):
				}
			}()

			if err := s.Turn(context.Background(), tc.followup); err != nil {
				t.Fatalf("Turn(working): %v", err)
			}

			// History grew monotonically (continue, not restart): both turns present.
			s.mu.Lock()
			gotHist := text(s.History)
			s.mu.Unlock()
			if len(gotHist) != 2 || gotHist[1] != tc.followup {
				t.Fatalf("History = %v, want [build it %s]", gotHist, tc.followup)
			}

			// Still Working — the follow-up did not launch a second drive.
			if s.PhaseNow() != Working {
				t.Fatalf("phase = %v after follow-up, want working", s.PhaseNow())
			}
			if rt.calls != 1 {
				t.Fatalf("router calls = %d, want 1 (follow-up must not re-route)", rt.calls)
			}

			// Steer mode fires the steer signal; queue mode must not.
			fired := false
			select {
			case <-steered:
				fired = true
			case <-time.After(50 * time.Millisecond):
			}
			if tc.wantMode == inbox.Steer && !fired {
				t.Fatal("steer follow-up did not fire the steer signal")
			}
			if tc.wantMode == inbox.Queue && fired {
				t.Fatal("queue follow-up fired the steer signal")
			}

			// The running drive drains the queued/steered message at its boundary.
			close(drv.release)
			s.Wait()
			if got := atomic.LoadInt32(&drv.drained); got != 1 {
				t.Fatalf("drive drained %d messages, want 1", got)
			}
		})
	}
}

// --- Routing failures leave the Session in Idle, never wedged -------------------

func TestRoutingFailuresReturnToIdle(t *testing.T) {
	t.Run("no router", func(t *testing.T) {
		s := New("c", "local", "/repo", nil)
		if err := s.Turn(context.Background(), "go"); !errors.Is(err, errNoRouter) {
			t.Fatalf("err = %v, want errNoRouter", err)
		}
		if s.PhaseNow() != Idle {
			t.Fatalf("phase = %v, want Idle", s.PhaseNow())
		}
	})

	t.Run("router error", func(t *testing.T) {
		rt := &fakeRouter{err: errors.New("classify failed")}
		s := New("c", "local", "/repo", nil)
		s.Router = rt
		if err := s.Turn(context.Background(), "go"); err == nil {
			t.Fatal("expected router error")
		}
		if s.PhaseNow() != Idle {
			t.Fatalf("phase = %v, want Idle", s.PhaseNow())
		}
	})

	t.Run("no driver for route", func(t *testing.T) {
		rt := &fakeRouter{route: RouteProject} // no Project driver wired
		s := New("c", "local", "/repo", nil)
		s.Router = rt
		if err := s.Turn(context.Background(), "go"); !errors.Is(err, errNoDriver) {
			t.Fatalf("err = %v, want errNoDriver", err)
		}
		if s.PhaseNow() != Idle {
			t.Fatalf("phase = %v, want Idle", s.PhaseNow())
		}
	})
}

// --- RouteContinue re-enters the driver named by State.Active -------------------

func TestRouteContinueUsesActiveDriver(t *testing.T) {
	rt := &fakeRouter{route: RouteContinue}
	drv := newFakeDriver(DriveResult{Verified: true})
	s := New("c", "local", "/repo", nil)
	s.Router = rt
	s.Drivers = Drivers{Supervise: drv}
	// Prior drive left the supervisor active.
	s.State.Active = RouteSupervise

	if err := s.Turn(context.Background(), "keep going"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	waitClosed(t, drv.started)
	if in := drv.input(); in.Route != RouteContinue {
		t.Fatalf("drive Route = %v, want continue", in.Route)
	}
	close(drv.release)
	s.Wait()
}

// --- Concurrency: History/Phase race-free under concurrent Turn calls -----------

func TestConcurrentTurnsRaceFree(t *testing.T) {
	rt := &fakeRouter{route: RouteNative}
	// Driver that releases itself immediately, so drives churn Idle↔Working while
	// concurrent Turns hammer the lock. -race is the real assertion here.
	drv := &selfReleasingDriver{}
	s := New("c", "local", "/repo", nil)
	s.Router = rt
	s.Drivers = Drivers{Native: drv}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = s.Turn(context.Background(), "msg")
		}(i)
	}
	wg.Wait()
	s.Wait()

	// No torn state: History is non-empty and Phase is a valid resting state.
	s.mu.Lock()
	n := len(s.History)
	s.mu.Unlock()
	if n != 50 {
		t.Fatalf("History len = %d, want 50 (every Turn appended exactly once)", n)
	}
	if p := s.PhaseNow(); p != Idle && p != Working && p != Routing {
		t.Fatalf("resting phase = %v, want a valid state", p)
	}
}

// selfReleasingDriver returns immediately, so Idle↔Working churns fast under load.
type selfReleasingDriver struct{}

func (selfReleasingDriver) Drive(_ context.Context, _ DriveInput) (DriveResult, error) {
	return DriveResult{Verified: true}, nil
}

// --- A failing drive still returns to Idle and does not fold State --------------

func TestFailedDriveReturnsToIdleWithoutFold(t *testing.T) {
	rt := &fakeRouter{route: RouteNative}
	drv := newFakeDriver(DriveResult{Summary: summarize.ContextSummary{Goal: "should not fold"}})
	drv.err = errors.New("drive blew up")
	s := New("c", "local", "/repo", nil)
	s.Router = rt
	s.Drivers = Drivers{Native: drv}

	if err := s.Turn(context.Background(), "go"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	waitClosed(t, drv.started)
	close(drv.release)
	s.Wait()

	waitPhase(t, s, Idle)
	if s.State.Summary.Goal == "should not fold" {
		t.Fatal("State folded a failed drive's result")
	}
}
