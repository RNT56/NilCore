package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"nilcore/internal/emit"
	"nilcore/internal/inbox"
	"nilcore/internal/model"
	"nilcore/internal/summarize"
)

// recordingEmitter captures every emitted event so a test can assert reasoning /
// steer-ack surfacing reached the sink. It is concurrency-safe (the loop/driver
// may emit from a drive goroutine).
type recordingEmitter struct {
	mu     sync.Mutex
	events []emit.Event
}

func (e *recordingEmitter) Emit(ev emit.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func (e *recordingEmitter) all() []emit.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]emit.Event, len(e.events))
	copy(out, e.events)
	return out
}

// --- RouteNative: History→Seed, Inbox + Emitter wired through, verifier verdict --

func TestNativeDriverWiresInboxSeedEmitter(t *testing.T) {
	// The wiring closure captures everything it was handed, then surfaces a
	// reasoning line through the Emitter and drains the Inbox to prove both seams
	// are live. It returns a verifier-VERIFIED outcome (I2: the verdict, not a
	// self-claim).
	var got NativeRun
	var drained int
	run := func(_ context.Context, in NativeRun) (DriveOutcome, error) {
		got = in
		if in.Emitter != nil {
			in.Emitter.Emit(emit.Event{Kind: emit.KindIntent, Step: 0, Text: "thinking"})
		}
		if in.Inbox != nil {
			drained = len(in.Inbox.Drain())
		}
		return DriveOutcome{Summary: "fixed the typo", Branch: "work-7", Verified: true}, nil
	}

	d := NewNativeDriver(run, nil, "chat-local")
	em := &recordingEmitter{}
	box := inbox.New(nil, "chat-local")
	box.Push(userTurn("also lower-case it"), inbox.Queue) // a queued mid-work turn

	history := []model.Message{userTurn("fix the typo")}
	res, err := d.Drive(context.Background(), DriveInput{
		Route:   RouteNative,
		Goal:    "fix the typo",
		History: history,
		Inbox:   box,
		Out:     em,
	})
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}

	// History was passed as the loop's Seed (continue, not restart).
	if len(got.Seed) != 1 || textOf(got.Seed[0]) != "fix the typo" {
		t.Fatalf("Seed = %v, want [fix the typo] (History threaded as Seed)", text(got.Seed))
	}
	// The live Inbox reached the loop and the queued turn was drainable there.
	if got.Inbox == nil {
		t.Fatal("Inbox not wired into the native run")
	}
	if drained != 1 {
		t.Fatalf("loop drained %d queued turns, want 1 (steer/queue reaches the running loop)", drained)
	}
	// Reasoning was surfaced through the Emitter.
	if !hasKind(em.all(), emit.KindIntent) {
		t.Fatal("no reasoning emitted through the wired Emitter")
	}
	// The per-drive task id is the worktree/eventlog key (id+"-"+seq) — NOT the
	// budget key; the conversation id is the prefix.
	if !strings.HasPrefix(got.TaskID, "chat-local-") {
		t.Fatalf("TaskID = %q, want a chat-local-<seq> worktree key", got.TaskID)
	}
	// The verifier verdict (I2) and the integration tip fold through verbatim.
	if !res.Verified {
		t.Fatal("DriveResult.Verified = false, want the closure's verifier verdict (true)")
	}
	if res.Branch != "work-7" {
		t.Fatalf("Branch = %q, want work-7", res.Branch)
	}
}

// TestNativeDriverUnverifiedFoldsFalse asserts a verifier-RED outcome folds back
// Verified=false — the driver never upgrades a self-claim to done (I2).
func TestNativeDriverUnverifiedFoldsFalse(t *testing.T) {
	run := func(_ context.Context, _ NativeRun) (DriveOutcome, error) {
		return DriveOutcome{Summary: "tried", Verified: false}, nil
	}
	d := NewNativeDriver(run, nil, "c")
	res, err := d.Drive(context.Background(), DriveInput{Route: RouteNative, Goal: "g"})
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if res.Verified {
		t.Fatal("Verified = true on a red outcome — the verifier verdict was overridden")
	}
}

// TestNativeDriverTaskIDsAreUniqueNotBudgetKey asserts each drive gets a distinct
// worktree task id (so worktrees never collide) while the conversation id stays the
// stable prefix — the budget-keying boundary (§6).
func TestNativeDriverTaskIDsAreUniqueNotBudgetKey(t *testing.T) {
	var ids []string
	run := func(_ context.Context, in NativeRun) (DriveOutcome, error) {
		ids = append(ids, in.TaskID)
		return DriveOutcome{Verified: true}, nil
	}
	d := NewNativeDriver(run, nil, "conv-9")
	for i := 0; i < 3; i++ {
		if _, err := d.Drive(context.Background(), DriveInput{Route: RouteNative, Goal: "g"}); err != nil {
			t.Fatalf("Drive %d: %v", i, err)
		}
	}
	if len(ids) != 3 || ids[0] == ids[1] || ids[1] == ids[2] || ids[0] == ids[2] {
		t.Fatalf("task ids not unique per drive: %v", ids)
	}
	for _, id := range ids {
		if !strings.HasPrefix(id, "conv-9-") {
			t.Fatalf("task id %q lost the conversation prefix", id)
		}
	}
}

// --- RouteSupervise: goal + Inbox + Emitter wired into super.Supervisor.Run ------

func TestSuperviseDriverWiresInboxAndOut(t *testing.T) {
	var gotGoal string
	var gotInbox InboxHandle
	var gotOut emit.Emitter
	run := func(_ context.Context, goal string, _ []model.Message, in InboxHandle, out emit.Emitter) (DriveOutcome, error) {
		gotGoal, gotInbox, gotOut = goal, in, out
		return DriveOutcome{Summary: "shipped feature", Branch: "integ-3", Verified: true}, nil
	}
	d := NewSuperviseDriver(run, nil)
	box := inbox.New(nil, "c")
	em := &recordingEmitter{}

	res, err := d.Drive(context.Background(), DriveInput{
		Route: RouteSupervise,
		Goal:  "add pagination",
		Inbox: box,
		Out:   em,
	})
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if gotGoal != "add pagination" {
		t.Fatalf("goal = %q", gotGoal)
	}
	if gotInbox == nil {
		t.Fatal("Inbox (second concurrent source) not wired into the supervisor")
	}
	if gotOut == nil {
		t.Fatal("Out (reasoning) not wired into the supervisor")
	}
	if !res.Verified || res.Branch != "integ-3" {
		t.Fatalf("fold = (%v,%q), want (true, integ-3)", res.Verified, res.Branch)
	}
}

// --- RouteProject: State.Summary seeds the loop's initial ContextSummary ----------

func TestProjectDriverSeedsSummary(t *testing.T) {
	var gotSeed summarize.ContextSummary
	run := func(_ context.Context, _ string, seed summarize.ContextSummary, out emit.Emitter) (DriveOutcome, error) {
		gotSeed = seed
		if out == nil {
			t.Error("project Out not wired")
		}
		return DriveOutcome{Summary: "scaffolded", Branch: "main-tip", Verified: true}, nil
	}
	d := NewProjectDriver(run, nil)
	st := WorkState{Summary: summarize.ContextSummary{Goal: "build a shortener", Remaining: "add tests"}}

	res, err := d.Drive(context.Background(), DriveInput{
		Route: RouteProject,
		Goal:  "build a shortener",
		State: st,
		Out:   &recordingEmitter{},
	})
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}
	// The carried WorkState summary seeded the loop (continue, not restart).
	if gotSeed.Goal != "build a shortener" || gotSeed.Remaining != "add tests" {
		t.Fatalf("seed = %+v, want the carried State.Summary", gotSeed)
	}
	if !res.Verified || res.Branch != "main-tip" {
		t.Fatalf("fold = (%v,%q)", res.Verified, res.Branch)
	}
}

// --- RouteChat: one metered Complete, ZERO loops/worktrees, reply surfaced --------

func TestChatDriverRunsNoLoop(t *testing.T) {
	cls := &scriptModel{reply: "I'm fixing the typo in the error string."}
	d := NewChatDriver(cls)
	em := &recordingEmitter{}

	history := []model.Message{userTurn("what are you doing?")}
	res, err := d.Drive(context.Background(), DriveInput{
		Route:   RouteChat,
		Goal:    "what are you doing?",
		History: history,
		State:   WorkState{Summary: summarize.ContextSummary{Goal: "fix the typo"}},
		Out:     em,
	})
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}
	// Exactly ONE model call — no loop iterating, no verifier, no worktree.
	if cls.calls != 1 {
		t.Fatalf("chat made %d model calls, want exactly 1 (no loop)", cls.calls)
	}
	// The reply was surfaced through the Emitter and recorded as the outcome tail.
	if got := em.all(); len(got) != 1 || got[0].Text != cls.reply {
		t.Fatalf("emitted = %v, want one reply line", got)
	}
	if res.Outcome != cls.reply {
		t.Fatalf("Outcome = %q, want the reply", res.Outcome)
	}
	// Chat does no work: Verified is vacuously true (no gate failed) and the prior
	// goal is carried forward so a later RouteContinue still references it.
	if !res.Verified {
		t.Fatal("chat Verified = false (there was no gate to fail)")
	}
	if res.Summary.Goal != "fix the typo" {
		t.Fatalf("chat dropped the carried goal: %q", res.Summary.Goal)
	}
}

// --- Unwired drivers return a structured error, never a panic --------------------

func TestUnwiredDriversError(t *testing.T) {
	in := DriveInput{Goal: "g"}
	for _, tc := range []struct {
		name string
		d    Driver
	}{
		{"native", NewNativeDriver(nil, nil, "c")},
		{"supervise", NewSuperviseDriver(nil, nil)},
		{"project", NewProjectDriver(nil, nil)},
		{"chat", NewChatDriver(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.d.Drive(context.Background(), in); err == nil {
				t.Fatal("unwired driver returned no error")
			}
		})
	}
}

// TestDriverPropagatesRunError asserts a closure error is wrapped and returned (the
// Session then returns to Idle without folding — covered in session_test.go).
func TestDriverPropagatesRunError(t *testing.T) {
	boom := errors.New("backend exploded")
	d := NewNativeDriver(func(context.Context, NativeRun) (DriveOutcome, error) {
		return DriveOutcome{}, boom
	}, nil, "c")
	_, err := d.Drive(context.Background(), DriveInput{Route: RouteNative, Goal: "g"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the wrapped run error", err)
	}
}

// --- End-to-end through Session.Turn: route → drive → Inbox reaches loop → fold ---

// TestSessionDrivesAndSteerReachesLoop wires a REAL Session to a REAL nativeDriver
// (with a fake run closure) and drives a full Idle→Working→Idle cycle, asserting
// the integration the task calls out: the route's machinery is invoked with the
// session Inbox + Emitter passed through, a mid-work steer reaches the running
// loop, and the terminal result folds back into WorkState.
func TestSessionDrivesAndSteerReachesLoop(t *testing.T) {
	// The run closure blocks until the test pushes a steer, then drains it — proving
	// a mid-work message reaches the live loop — and surfaces a steer-ack.
	steered := make(chan struct{})
	gotSteer := make(chan int, 1)
	run := func(_ context.Context, in NativeRun) (DriveOutcome, error) {
		<-steered // wait for the test's mid-work steer
		n := 0
		if in.Inbox != nil {
			n = len(in.Inbox.Drain())
		}
		gotSteer <- n
		if in.Emitter != nil {
			in.Emitter.Emit(emit.Event{Kind: emit.KindSteerAck, Text: "folding your message in"})
		}
		return DriveOutcome{Summary: "done", Branch: "b1", Verified: true}, nil
	}

	em := &recordingEmitter{}
	s := New("chat-local", "local", "/repo", nil)
	s.Router = &fakeRouter{route: RouteNative}
	s.Drivers = Drivers{Native: NewNativeDriver(run, nil, s.ID)}
	s.Out = em

	if err := s.Turn(context.Background(), "fix the bug"); err != nil {
		t.Fatalf("Turn(idle): %v", err)
	}
	waitPhase(t, s, Working)

	// Mid-work steer: Turn returns immediately and pushes to the live Inbox.
	if err := s.Turn(context.Background(), "!actually rename it"); err != nil {
		t.Fatalf("Turn(working): %v", err)
	}
	close(steered) // release the drive so it drains
	s.Wait()

	if n := <-gotSteer; n != 1 {
		t.Fatalf("running loop drained %d mid-work turns, want 1 (steer reached the loop)", n)
	}
	if !hasKind(em.all(), emit.KindSteerAck) {
		t.Fatal("steer-ack not surfaced through the session Emitter")
	}

	// The terminal result folded into WorkState; Phase returned to Idle.
	waitPhase(t, s, Idle)
	if s.State.Branch != "b1" {
		t.Fatalf("State.Branch = %q after fold, want b1", s.State.Branch)
	}
	if s.State.Active != RouteNative {
		t.Fatalf("State.Active = %v, want native", s.State.Active)
	}
	// History carries BOTH turns (continue, not restart): the original goal and the
	// steer — the loop got the goal as Seed; the steer reached it via the Inbox.
	s.mu.Lock()
	hist := text(s.History)
	s.mu.Unlock()
	if len(hist) != 2 || hist[0] != "fix the bug" || hist[1] != "!actually rename it" {
		t.Fatalf("History = %v, want [fix the bug !actually rename it]", hist)
	}
}

// TestSessionContinueReentersActiveDriver asserts the RouteContinue path re-enters
// the driver named by State.Active with the appended History — the persistence
// requirement, end-to-end through the Session.
func TestSessionContinueReentersActiveDriver(t *testing.T) {
	var seedLen int
	// The closure proves which driver ran: only the Supervise field is wired, so a
	// RouteContinue that re-enters the WRONG driver would surface as errNoDriver
	// (the Native/Project/Chat fields are nil), never reach this closure, and leave
	// seedLen at 0.
	ran := make(chan struct{}, 1)
	run := func(_ context.Context, _ string, seed []model.Message, _ InboxHandle, _ emit.Emitter) (DriveOutcome, error) {
		seedLen = len(seed)
		ran <- struct{}{}
		return DriveOutcome{Summary: "more done", Verified: true}, nil
	}
	s := New("c", "local", "/repo", nil)
	s.Router = &fakeRouter{route: RouteContinue}
	s.Drivers = Drivers{Supervise: NewSuperviseDriver(run, nil)}
	s.State.Active = RouteSupervise                                 // a prior supervise drive
	s.History = []model.Message{userTurn("the original feature")}   // prior turn
	s.State.Summary = summarize.ContextSummary{Goal: "the feature"} // bounded carry-over

	if err := s.Turn(context.Background(), "now also add tests"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	s.Wait()

	// RouteContinue resolved to the Supervise driver (named by State.Active) and ran
	// it, seeded with the appended History (prior turn + the continue turn) —
	// continue, not restart.
	select {
	case <-ran:
	default:
		t.Fatal("RouteContinue did not re-enter the Supervise driver (State.Active)")
	}
	if seedLen < 2 {
		t.Fatalf("continue drive seeded with %d turns, want the appended History (>=2)", seedLen)
	}
}

// --- foldOutcome: bounded fold, summarize fallback, verdict pass-through ----------

func TestFoldOutcomeWithoutModelKeepsGoalAndVerdict(t *testing.T) {
	res := foldOutcome(context.Background(), nil, "the goal",
		DriveOutcome{Summary: "did a thing", Branch: "tip", Verified: true})
	if res.Summary.Goal != "the goal" {
		t.Fatalf("Summary.Goal = %q, want the goal (nil-model fallback)", res.Summary.Goal)
	}
	if res.Summary.Remaining != "did a thing" {
		t.Fatalf("Summary.Remaining = %q, want the bounded outcome tail", res.Summary.Remaining)
	}
	if res.Branch != "tip" || !res.Verified {
		t.Fatalf("verdict/branch not passed through: %+v", res)
	}
	if res.Outcome != "did a thing" {
		t.Fatalf("Outcome = %q, want the data-only tail", res.Outcome)
	}
}

func TestFoldOutcomeUsesSummarizeWhenModelWired(t *testing.T) {
	// A model returning valid summarize JSON distils the outcome into the bounded
	// ContextSummary (reusing summarize's discipline) rather than the raw tail.
	cls := &scriptModel{reply: `{"goal":"the goal","decisions":["chose X"],"remaining":"wire Y"}`}
	res := foldOutcome(context.Background(), cls, "the goal",
		DriveOutcome{Summary: "long machine account", Verified: true})
	if len(res.Summary.Decisions) != 1 || res.Summary.Decisions[0] != "chose X" {
		t.Fatalf("Summary.Decisions = %v, want the summarized decision", res.Summary.Decisions)
	}
	if res.Summary.Remaining != "wire Y" {
		t.Fatalf("Summary.Remaining = %q, want the summarized remainder", res.Summary.Remaining)
	}
	if cls.calls != 1 {
		t.Fatalf("summarize made %d calls, want 1", cls.calls)
	}
}

func TestFoldOutcomeSummarizeFailureDegrades(t *testing.T) {
	// A summarize transport error degrades to the minimal goal+tail summary rather
	// than failing the fold (the machine still produced a verified result).
	cls := &scriptModel{err: errors.New("nope")}
	res := foldOutcome(context.Background(), cls, "the goal",
		DriveOutcome{Summary: "tail", Verified: true})
	if res.Summary.Goal != "the goal" || res.Summary.Remaining != "tail" {
		t.Fatalf("degraded summary = %+v, want goal+tail", res.Summary)
	}
	if !res.Verified {
		t.Fatal("verdict lost on summarize failure")
	}
}

// --- helpers --------------------------------------------------------------------

func textOf(m model.Message) string {
	var s string
	for _, b := range m.Content {
		s += b.Text
	}
	return s
}

func hasKind(events []emit.Event, kind string) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
