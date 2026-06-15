package super

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"nilcore/internal/agent/bus"
	"nilcore/internal/emit"
	"nilcore/internal/inbox"
	"nilcore/internal/model"
	"nilcore/internal/roster"
	"nilcore/internal/spawn"
)

// --- ST-T07: stream the supervisor (planner) loop + interrupt-but-preserve ---
//
// These tests mirror backend/native_stream_test.go for the supervisor. The path
// under test is the streaming planner turn the loop takes when Out is wired AND
// s.Model implements model.Streamer. The decisive properties:
//   - planner text deltas reach Out as KindToken events, in order;
//   - a steer mid-stream INTERRUPTS the Stream but PRESERVES its partial planner
//     reasoning TEXT as the assistant turn (dropping any incomplete tool_use), folds
//     the feedback as an un-Wrap'd PRINCIPAL turn, emits a steer_ack, continues NO error;
//   - a shutdown (parent-ctx cancel) mid-stream aborts cleanly (reason "ctx");
//   - Out==nil OR a non-streaming provider is byte-identical (the Complete path);
//   - the per-round stream-watcher and the dedicated bus-reader coexist (no leak/deadlock):
//     the watcher cancels ONLY the per-round child ctx, never the task ctx the reader uses.
// All concurrency uses real goroutines + channels and runs under `go test -race`.

// superStreamStep is one scripted streaming response for the planner: the text
// deltas to forward (in order) and the full Response to return on a clean
// end-of-stream. honorCtx parks after the deltas until released-or-cancelled
// (modeling a real provider interrupted mid-generation, returning the PARTIAL
// Response + ctx.Err()); blockClean parks IGNORING ctx so the step always returns
// cleanly (full, nil) even when a steer cancels streamCtx — the finish-line case.
type superStreamStep struct {
	deltas     []string
	full       model.Response
	partial    model.Response
	honorCtx   bool
	blockClean bool
}

// fakeSuperStreamer is a model.Provider that also implements model.Streamer,
// mirroring the native fakeStreamer. Each Stream call plays the next scripted step;
// Complete satisfies model.Provider for the non-streaming/nil-Out gate tests.
type fakeSuperStreamer struct {
	mu       sync.Mutex
	steps    []superStreamStep
	i        int
	lastMsgs []model.Message
	entered  chan int      // signals which Stream call has parked (honorCtx/blockClean)
	release  chan struct{} // lets a parked Stream complete
}

func (f *fakeSuperStreamer) Model() string { return "fake-super-streamer" }

func (f *fakeSuperStreamer) Complete(_ context.Context, _ string, msgs []model.Message, _ []model.Tool, _ int) (model.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastMsgs = msgs
	if f.i >= len(f.steps) {
		return model.Response{StopReason: "end_turn"}, nil
	}
	s := f.steps[f.i]
	f.i++
	return s.full, nil
}

func (f *fakeSuperStreamer) Stream(ctx context.Context, _ string, msgs []model.Message, _ []model.Tool, _ int, onChunk func(model.Chunk)) (model.Response, error) {
	f.mu.Lock()
	f.lastMsgs = msgs
	n := f.i
	if n >= len(f.steps) {
		f.mu.Unlock()
		return model.Response{StopReason: "end_turn"}, nil
	}
	s := f.steps[n]
	f.i++
	f.mu.Unlock()

	for _, d := range s.deltas {
		if err := ctx.Err(); err != nil {
			return s.partial, err
		}
		if onChunk != nil {
			onChunk(model.Chunk{Text: d})
		}
	}

	if s.honorCtx {
		if f.entered != nil {
			select {
			case f.entered <- n:
			default:
			}
		}
		select {
		case <-ctx.Done():
			return s.partial, ctx.Err()
		case <-f.release:
		}
	}
	if s.blockClean {
		if f.entered != nil {
			select {
			case f.entered <- n:
			default:
			}
		}
		<-f.release // park IGNORING ctx → always returns cleanly (the finish-line case)
	}
	return s.full, nil
}

func (f *fakeSuperStreamer) history() []model.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastMsgs
}

// streamTextResp builds the full Response a step returns on clean completion.
func streamTextResp(blocks ...model.Block) model.Response {
	return model.Response{Content: blocks, StopReason: "tool_use"}
}

// superTokenTexts returns the Text of every KindToken event the emitter recorded,
// in order — the live token stream the loop forwarded.
func superTokenTexts(r *recordingEmitter) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		if e.Kind == emit.KindToken {
			out = append(out, e.Text)
		}
	}
	return out
}

// hasAssistantText reports whether any assistant message carries the given text.
func hasAssistantText(msgs []model.Message, text string) bool {
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, text) {
				return true
			}
		}
	}
	return false
}

// TestSuperStreamForwardsTokensInOrder asserts that with Out + a Streamer the
// supervisor takes the Stream path and forwards each planner text delta to Out as a
// KindToken event, in order.
func TestSuperStreamForwardsTokensInOrder(t *testing.T) {
	f := &fakeSuperStreamer{steps: []superStreamStep{
		{
			deltas: []string{"I'll ", "decompose ", "the goal"},
			full: streamTextResp(
				model.Block{Type: "text", Text: "I'll decompose the goal"},
				toolUse("u1", "finish", map[string]string{"summary": "done"})),
		},
	}}
	em := &recordingEmitter{}
	s := baseSup(f, passVerifier{})
	s.Out = em
	if _, err := s.Run(context.Background(), "goal"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := superTokenTexts(em)
	want := []string{"I'll ", "decompose ", "the goal"}
	if len(got) != len(want) {
		t.Fatalf("token deltas = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token %d = %q, want %q (order broken)", i, got[i], want[i])
		}
	}
}

// TestSuperStreamSteerInterruptsButPreserves is the core ST-T07 acceptance test: a
// steer mid-stream INTERRUPTS the planner's Stream, but the partial reasoning TEXT
// is preserved as the assistant turn (with NO partial tool_use), the feedback folds
// as an un-Wrap'd PRINCIPAL turn, a steer_ack is emitted, and the loop continues
// with no error — the planner re-thinks next round and finishes.
func TestSuperStreamSteerInterruptsButPreserves(t *testing.T) {
	var spawnCalled bool
	var smu sync.Mutex
	spawnFn := func(_ context.Context, spec SubagentSpec) spawn.Result {
		smu.Lock()
		spawnCalled = true
		smu.Unlock()
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
	}

	f := &fakeSuperStreamer{
		entered: make(chan int, 4),
		release: make(chan struct{}),
		steps: []superStreamStep{
			// round 0: streams partial reasoning then PARKS (honorCtx). A steer cancels
			// it; the partial carries TEXT plus an incomplete spawn_subagent tool_use that
			// MUST be dropped (the planner was mid-call). The kept text is the assistant turn.
			{
				deltas:   []string{"Planning ", "the split"},
				honorCtx: true,
				partial: model.Response{
					Content: []model.Block{
						{Type: "text", Text: "Planning the split"},
						toolUse("partial-uX", "spawn_subagent", SubagentSpec{ID: "super.t1", Role: roster.RoleImplementer, Goal: "build"}),
					},
				},
			},
			// round 1: after the steer fold, the planner reconsiders and finishes.
			{
				full: streamTextResp(toolUse("u2", "finish", map[string]string{"summary": "done"})),
			},
		},
	}
	ib := inbox.New(nil, "conv")
	em := &recordingEmitter{}
	s := baseSup(f, passVerifier{})
	s.Inbox = ib
	s.Out = em
	s.Spawn = spawnFn

	done := make(chan error, 1)
	go func() { _, err := s.Run(context.Background(), "goal"); done <- err }()

	<-f.entered // round 0 streamed its deltas and parked inside Stream
	// Steer NOW: it must cancel the in-flight Stream (interrupt) and fold at the
	// boundary. Pushing before any release makes the steer the cancellation cause.
	ib.Push(userText("split by layer, not by file"), inbox.Steer)

	if err := <-done; err != nil {
		t.Fatalf("Run errored on a mid-stream steer (steer misread as fault/shutdown): %v", err)
	}

	hist := f.history()
	if !hasUserText(hist, "split by layer, not by file") {
		t.Error("steer text not folded as a user turn after the interrupt")
	}
	// It must be the PRINCIPAL block (un-Wrap'd), not fenced as data (the trust line, I7).
	if !principalUnfenced(hist, "split by layer, not by file") {
		t.Error("steered principal message was fenced as data — trust line broken (I7)")
	}
	// The partial reasoning TEXT must be preserved as an assistant turn.
	if !hasAssistantText(hist, "Planning the split") {
		t.Error("partial planner reasoning text was NOT preserved as the assistant turn")
	}
	// The incomplete tool_use must have been DROPPED — never appended.
	for _, m := range hist {
		for _, b := range m.Content {
			if b.Type == "tool_use" && b.ID == "partial-uX" {
				t.Error("incomplete partial tool_use was preserved — it must be dropped")
			}
		}
	}
	// The held spawn must NEVER have run.
	smu.Lock()
	if spawnCalled {
		smu.Unlock()
		t.Error("partial spawn executed despite the interrupt")
	} else {
		smu.Unlock()
	}
	var sawAck bool
	for _, k := range em.kinds() {
		if k == emit.KindSteerAck {
			sawAck = true
		}
	}
	if !sawAck {
		t.Error("no steer_ack emitted after a mid-stream interrupt")
	}
}

// TestSuperStreamFinishLineSteerStillPauses pins the finish-line race: a steer the
// stream-watcher consumes JUST as Stream returns CLEANLY (err==nil) must NOT be
// dropped — the loop carries it into the post-completion CV-T01 pause, so the full
// proposed action is HELD (never run) and the feedback folded.
func TestSuperStreamFinishLineSteerStillPauses(t *testing.T) {
	var spawnCalled bool
	var smu sync.Mutex
	spawnFn := func(_ context.Context, spec SubagentSpec) spawn.Result {
		smu.Lock()
		spawnCalled = true
		smu.Unlock()
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
	}

	f := &fakeSuperStreamer{
		entered: make(chan int, 4),
		release: make(chan struct{}),
		steps: []superStreamStep{
			// round 0: streams a delta, signals entered, parks IGNORING ctx so it returns
			// CLEANLY even after the steer cancels streamCtx. Its full carries a spawn the
			// CV-T01 pause must HOLD (never run).
			{
				deltas:     []string{"deciding"},
				blockClean: true,
				full: streamTextResp(
					model.Block{Type: "text", Text: "deciding"},
					toolUse("u1", "spawn_subagent", SubagentSpec{ID: "super.t1", Role: roster.RoleImplementer, Goal: "build"})),
			},
			// round 1: after the steer fold, the planner reconsiders and finishes.
			{
				full: streamTextResp(toolUse("u2", "finish", map[string]string{"summary": "done"})),
			},
		},
	}
	ib := inbox.New(nil, "conv")
	em := &recordingEmitter{}
	s := baseSup(f, passVerifier{})
	s.Inbox = ib
	s.Out = em
	s.Spawn = spawnFn

	done := make(chan error, 1)
	go func() { _, err := s.Run(context.Background(), "goal"); done <- err }()

	<-f.entered // round 0 parked (blockClean), about to return cleanly
	ib.Push(userText("hold on — change of plan"), inbox.Steer)
	time.Sleep(20 * time.Millisecond) // bias the watcher to consume the SIGNAL first
	close(f.release)                  // round 0 returns full,nil; round 1 (no park) also uses this closed chan

	if err := <-done; err != nil {
		t.Fatalf("Run errored on a finish-line steer: %v", err)
	}
	hist := f.history()
	if !hasUserText(hist, "hold on — change of plan") {
		t.Error("finish-line steer was dropped — not folded as a user turn")
	}
	if !hasToolResult(hist, "Paused before this ran") {
		t.Error("finish-line steer did not HOLD the proposed action (CV-T01 pause skipped)")
	}
	smu.Lock()
	if spawnCalled {
		smu.Unlock()
		t.Error("held spawn executed despite the finish-line steer")
	} else {
		smu.Unlock()
	}
	var sawAck bool
	for _, k := range em.kinds() {
		if k == emit.KindSteerAck {
			sawAck = true
		}
	}
	if !sawAck {
		t.Error("no steer_ack emitted for the finish-line steer")
	}
}

// TestSuperStreamShutdownAbortsCleanly asserts a parent-ctx cancel (shutdown /
// deadline) while a planner Stream is in flight aborts cleanly — classified as a
// SHUTDOWN, never a steer (continue past the rail) nor a fault (transport error).
// Run returns no error and an un-Done Outcome with reason "ctx".
func TestSuperStreamShutdownAbortsCleanly(t *testing.T) {
	f := &fakeSuperStreamer{
		entered: make(chan int, 2),
		release: make(chan struct{}),
		steps: []superStreamStep{
			{
				deltas:   []string{"streaming "},
				honorCtx: true,
				partial:  model.Response{Content: []model.Block{{Type: "text", Text: "streaming "}}},
			},
		},
	}
	ib := inbox.New(nil, "conv")
	em := &recordingEmitter{}
	s := baseSup(f, passVerifier{})
	s.Inbox = ib
	s.Out = em

	ctx, cancel := context.WithCancel(context.Background())
	type res struct {
		out Outcome
		err error
	}
	dch := make(chan res, 1)
	go func() { o, e := s.Run(ctx, "goal"); dch <- res{o, e} }()

	<-f.entered // round 0 parked inside Stream
	cancel()    // shutdown the parent ctx mid-stream

	select {
	case r := <-dch:
		if r.err != nil {
			t.Fatalf("parent-cancel must return a clean Outcome, not an error: %v", r.err)
		}
		if r.out.Done {
			t.Error("a shutdown must not report Done")
		}
		if r.out.Reason != "ctx" {
			t.Errorf("shutdown reason = %q, want ctx", r.out.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not abort on parent-ctx cancel during a stream (shutdown not honored)")
	}
}

// TestSuperStreamWatcherAndBusReaderCoexist is the decisive coexistence test: with a
// Streamer + Out AND a real Bus wired, the per-round stream-watcher and the dedicated
// bus-reader run concurrently. A steer mid-stream cancels ONLY the per-round child ctx
// — never the task ctx the bus-reader drains under — so a subagent's blocking Ask is
// still answered ACROSS the steer, and the loop runs to a clean finish with no leak or
// deadlock.
func TestSuperStreamWatcherAndBusReaderCoexist(t *testing.T) {
	base := waitGoroutines(runtime.NumGoroutine())

	b := bus.New(nil, 8, 0)
	// Register the subagent's mailbox so the supervisor's answer routes back to it.
	if _, err := bus.NewPeer(b, "super.t1"); err != nil {
		t.Fatalf("NewPeer: %v", err)
	}

	f := &fakeSuperStreamer{
		entered: make(chan int, 4),
		release: make(chan struct{}),
		steps: []superStreamStep{
			// round 0: stream + park (honorCtx) so a steer interrupts it mid-stream.
			{
				deltas:   []string{"thinking"},
				honorCtx: true,
				partial:  model.Response{Content: []model.Block{{Type: "text", Text: "thinking"}}},
			},
			// round 1: after the steer fold, finish.
			{
				full: streamTextResp(toolUse("u2", "finish", map[string]string{"summary": "done"})),
			},
		},
	}
	ib := inbox.New(nil, "conv")
	em := &recordingEmitter{}
	s := baseSup(f, passVerifier{})
	s.Inbox = ib
	s.Out = em
	s.Bus = b // starts the dedicated bus-reader alongside the per-round stream-watcher
	// A graceful Answer hook so the subagent's Ask resolves promptly and we can assert
	// the bus-reader kept answering across the steer.
	s.Answer = func(context.Context, bus.Message) string { return "proceed within scope" }

	done := make(chan error, 1)
	go func() { _, err := s.Run(context.Background(), "goal"); done <- err }()

	<-f.entered // round 0 streamed + parked inside Stream

	// While the planner's Stream is parked, a subagent blocks on Ask. The dedicated
	// bus-reader (under the TASK ctx) must answer it even though the supervisor goroutine
	// is inside Stream. Run the Ask on its own goroutine; it should resolve quickly.
	askDone := make(chan string, 1)
	go func() {
		askCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		reply, err := b.Ask(askCtx, bus.Message{
			Sender: "super.t1", To: []bus.AgentID{bus.Supervisor},
			Kind: bus.KindQuestion, TTL: 4, Payload: "which API version?",
		})
		if err != nil {
			askDone <- "ERR: " + err.Error()
			return
		}
		askDone <- reply.Payload
	}()

	select {
	case reply := <-askDone:
		if !strings.Contains(reply, "proceed within scope") {
			t.Errorf("bus-reader answered with %q, want the Answer-hook reply", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subagent Ask was not answered while the planner streamed — bus-reader stalled")
	}

	// Now steer mid-stream. The watcher cancels ONLY the per-round child ctx; the
	// bus-reader (task ctx) must be untouched. The loop folds and continues to finish.
	ib.Push(userText("change the plan"), inbox.Steer)

	if err := <-done; err != nil {
		t.Fatalf("Run errored with a bus-reader + stream-watcher both live: %v", err)
	}
	if g := waitGoroutines(base); g > base {
		t.Errorf("watcher/reader leaked goroutines: baseline %d, after %d", base, g)
	}
}

// TestSuperStreamNoWatcherGoroutineLeak asserts that across a multi-round streaming
// loop (a watcher spawns each round when Out is wired) every watcher is torn down and
// the goroutine count returns to baseline — no leak.
func TestSuperStreamNoWatcherGoroutineLeak(t *testing.T) {
	base := waitGoroutines(runtime.NumGoroutine())
	f := &fakeSuperStreamer{steps: []superStreamStep{
		{deltas: []string{"a"}, full: streamTextResp(model.Block{Type: "text", Text: "a"}, toolUse("u1", "plan", map[string]string{"goal": "a"}))},
		{deltas: []string{"b"}, full: streamTextResp(model.Block{Type: "text", Text: "b"}, toolUse("u2", "plan", map[string]string{"goal": "b"}))},
		{deltas: []string{"c"}, full: streamTextResp(model.Block{Type: "text", Text: "c"}, toolUse("u3", "finish", map[string]string{"summary": "done"}))},
	}}
	ib := inbox.New(nil, "conv")
	em := &recordingEmitter{}
	s := baseSup(f, passVerifier{})
	s.Inbox = ib
	s.Out = em
	if _, err := s.Run(context.Background(), "goal"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if g := waitGoroutines(base); g > base {
		t.Errorf("stream watcher goroutine leaked: baseline %d, after %d", base, g)
	}
}

// TestSuperStreamerWithNilOutUsesComplete asserts the gate: a Streamer provider with
// NO Out wired takes the Complete (non-streaming) path — byte-identical. No KindToken
// events (there is no sink), and the loop still drives to a normal converged finish.
func TestSuperStreamerWithNilOutUsesComplete(t *testing.T) {
	f := &fakeSuperStreamer{steps: []superStreamStep{
		{
			// Deltas would only flow on the Stream path; Complete returns full directly.
			deltas: []string{"never ", "streamed"},
			full:   streamTextResp(toolUse("u1", "finish", map[string]string{"summary": "done"})),
		},
	}}
	// No Out → Complete path even though f is a Streamer.
	s := baseSup(f, passVerifier{})
	out, err := s.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done {
		t.Error("streamer-with-nil-Out loop should converge via the Complete path")
	}
}

// nonStreamerSuper is a plain model.Provider with NO Stream method, used to assert
// the loop falls back to Complete even when Out is wired (no streaming token events)
// — byte-identical to the existing emit-only behavior, the KindIntent surface intact.
type nonStreamerSuper struct {
	responses []model.Response
	i         int
}

func (s *nonStreamerSuper) Model() string { return "non-streamer-super" }
func (s *nonStreamerSuper) Complete(context.Context, string, []model.Message, []model.Tool, int) (model.Response, error) {
	if s.i >= len(s.responses) {
		return model.Response{StopReason: "end_turn"}, nil
	}
	r := s.responses[s.i]
	s.i++
	return r, nil
}

// TestSuperNonStreamerWithOutUsesComplete asserts the other half of the gate: Out
// wired but a provider that does NOT implement Streamer takes the Complete path — no
// KindToken events, the existing KindIntent reasoning surface still fires.
func TestSuperNonStreamerWithOutUsesComplete(t *testing.T) {
	m := &nonStreamerSuper{responses: []model.Response{
		{Content: []model.Block{{Type: "text", Text: "thinking"}, toolUse("u1", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	em := &recordingEmitter{}
	s := baseSup(m, passVerifier{})
	s.Out = em
	if _, err := s.Run(context.Background(), "goal"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if toks := superTokenTexts(em); len(toks) != 0 {
		t.Errorf("non-streamer produced KindToken events: %v", toks)
	}
	var sawIntent bool
	for _, k := range em.kinds() {
		if k == emit.KindIntent {
			sawIntent = true
		}
	}
	if !sawIntent {
		t.Error("non-streamer+Out path lost the KindIntent reasoning surface")
	}
}
