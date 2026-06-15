package backend

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"nilcore/internal/emit"
	"nilcore/internal/inbox"
	"nilcore/internal/model"
)

// --- ST-T06: stream the native loop + interrupt-but-preserve ----------------
//
// These tests exercise the streaming model-call path the native loop takes when
// an Emitter is wired AND n.Model implements model.Streamer. The decisive
// properties under test:
//   - text deltas reach the Emitter as KindToken events, in order;
//   - a steer mid-stream INTERRUPTS the Stream but PRESERVES its partial reasoning
//     TEXT as the assistant turn (dropping any incomplete tool_use), folds the
//     feedback, emits a steer_ack, and continues with NO error;
//   - a shutdown (parent-ctx cancel) mid-stream aborts cleanly (interrupted Result);
//   - Emitter==nil OR a non-streaming provider is byte-identical (Complete path);
//   - no watcher goroutine leaks across iterations.
// All concurrency uses real goroutines + channels and runs under `go test -race`.

// streamStep is one scripted streaming response: the text deltas to forward (in
// order) and the full Response to return on a CLEAN end-of-stream. When honorCtx
// is set the step blocks after emitting its deltas until either released or ctx
// is cancelled — modeling a real provider that is interrupted mid-generation and
// returns the PARTIAL Response assembled so far together with ctx.Err().
type streamStep struct {
	deltas   []string       // text deltas forwarded to onChunk in order
	full     model.Response // returned on clean completion (deltas already sent)
	partial  model.Response // returned WITH ctx.Err() when cancelled mid-stream
	honorCtx bool           // block after deltas until released-or-cancelled
	// blockClean parks (after deltas) until released, IGNORING ctx — modeling a
	// stream that finishes CLEANLY (returns full, nil) right as a steer lands, so a
	// test can deterministically exercise the finish-line-steer fallback.
	blockClean bool
}

// fakeStreamer is a model.Provider that also implements model.Streamer. Each
// Stream call plays the next scripted streamStep: it forwards the step's deltas
// to onChunk in order, then either completes cleanly (returning full) or — for an
// honorCtx step — parks until the test releases it or ctx is cancelled, in which
// case it returns the partial Response + ctx.Err() (the interrupt-but-preserve
// contract). Complete is implemented so the type is a Provider too, but the loop
// must take the Stream path when an Emitter is wired.
type fakeStreamer struct {
	mu       sync.Mutex
	steps    []streamStep
	i        int
	lastMsgs []model.Message // history offered on the most recent Stream/Complete
	entered  chan int        // signals the test which Stream call has parked (honorCtx)
	release  chan struct{}   // lets a parked honorCtx Stream complete cleanly
}

func (f *fakeStreamer) Model() string { return "fake-streamer" }

// Complete satisfies model.Provider. The streaming loop never calls it when an
// Emitter is wired, but a non-streaming or nil-Emitter test exercises it; it
// returns the step's full Response without streaming.
func (f *fakeStreamer) Complete(_ context.Context, _ string, msgs []model.Message, _ []model.Tool, _ int) (model.Response, error) {
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

func (f *fakeStreamer) Stream(ctx context.Context, _ string, msgs []model.Message, _ []model.Tool, _ int, onChunk func(model.Chunk)) (model.Response, error) {
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

	// Forward the deltas in order, honoring ctx between them so a cancellation
	// that lands before a later delta still returns the partial assembled so far.
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
		// Park until released (clean completion) or ctx cancelled (interrupt). On
		// cancel return the PARTIAL Response + ctx.Err() — interrupt-but-preserve.
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
		// Park until released, IGNORING ctx: this step always returns CLEANLY (full,
		// nil), even if a steer cancelled streamCtx meanwhile — the finish-line case.
		<-f.release
	}
	return s.full, nil
}

// textBlock / toolUseBlock build content blocks for scripted responses.
func textBlock(s string) model.Block { return model.Block{Type: "text", Text: s} }

// tokenTexts returns the Text of every KindToken event the emitter recorded, in
// order — the live token stream the loop forwarded.
func tokenTexts(r *recordingEmitter) []string {
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

// TestNativeStreamForwardsTokensInOrder asserts that with an Emitter + a Streamer
// the loop takes the Stream path and forwards each text delta to the Emitter as a
// KindToken event, in order. The concatenation of the tokens equals the model's
// streamed prose.
func TestNativeStreamForwardsTokensInOrder(t *testing.T) {
	f := &fakeStreamer{steps: []streamStep{
		{
			deltas: []string{"I'll ", "inspect ", "the file"},
			full: model.Response{
				Content:    []model.Block{textBlock("I'll inspect the file"), toolUse("u1", "finish", map[string]string{"summary": "done"})},
				StopReason: "tool_use",
			},
		},
	}}
	em := &recordingEmitter{}
	n := &Native{Model: f, Box: &recordingBox{}, Verifier: okVerifier{}, Emitter: em, MaxSteps: 5}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := tokenTexts(em)
	want := []string{"I'll ", "inspect ", "the file"}
	if len(got) != len(want) {
		t.Fatalf("token deltas = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token %d = %q, want %q (order broken)", i, got[i], want[i])
		}
	}
}

// TestNativeStreamSteerInterruptsButPreserves is the core ST-T06 acceptance test:
// a steer that lands mid-stream INTERRUPTS the Stream, but the partial reasoning
// TEXT is preserved as the assistant turn (with NO partial tool_use), the feedback
// is folded as a user turn, a steer_ack is emitted, and the loop continues with no
// error — the model re-thinks next step.
func TestNativeStreamSteerInterruptsButPreserves(t *testing.T) {
	f := &fakeStreamer{
		entered: make(chan int, 4),
		release: make(chan struct{}),
		steps: []streamStep{
			// step 0: streams partial reasoning then PARKS (honorCtx). A steer cancels
			// it; the partial carries TEXT plus an incomplete tool_use that MUST be
			// dropped (the model was mid-call). The kept text becomes the assistant turn.
			{
				deltas:   []string{"Thinking about ", "the approach"},
				honorCtx: true,
				partial: model.Response{
					Content: []model.Block{
						textBlock("Thinking about the approach"),
						toolUse("partial-uX", "run", map[string]string{"cmd": "echo half-built"}),
					},
				},
			},
			// step 1: after the steer fold, the model reconsiders and finishes.
			{
				full: model.Response{
					Content:    []model.Block{toolUse("u2", "finish", map[string]string{"summary": "done"})},
					StopReason: "tool_use",
				},
			},
		},
	}
	ib := inbox.New(nil, "conv")
	em := &recordingEmitter{}
	box := &recordingBox{}
	n := &Native{Model: f, Box: box, Verifier: okVerifier{}, Inbox: ib, Emitter: em, MaxSteps: 5}

	done := make(chan error, 1)
	go func() { _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); done <- err }()

	<-f.entered // step 0 streamed its deltas and parked inside Stream
	// Steer NOW: it must cancel the in-flight Stream (interrupt) and fold at the
	// boundary. Pushing before any release makes the steer the cancellation cause.
	ib.Push(userText("use a different file"), inbox.Steer)

	// The loop classifies the cancel as a steer, preserves the partial text, folds
	// the feedback, and proceeds to step 1 (which streams to completion + finishes).
	if err := <-done; err != nil {
		t.Fatalf("Run errored on a mid-stream steer (steer misread as fault/shutdown): %v", err)
	}

	hist := f.lastHistory()
	if !hasUserText(hist, "use a different file") {
		t.Error("steer text not folded as a user turn after the interrupt")
	}
	// The partial reasoning TEXT must be preserved as an assistant turn.
	if !hasAssistantText(hist, "Thinking about the approach") {
		t.Error("partial reasoning text was NOT preserved as the assistant turn")
	}
	// The incomplete tool_use must have been DROPPED — never appended.
	for _, m := range hist {
		for _, b := range m.Content {
			if b.Type == "tool_use" && b.ID == "partial-uX" {
				t.Error("incomplete partial tool_use was preserved — it must be dropped")
			}
		}
	}
	// The half-built command must NEVER have executed.
	for _, c := range box.execed {
		if strings.Contains(c, "half-built") {
			t.Errorf("partial tool_use executed despite the interrupt: %q", c)
		}
	}
	// A steer_ack must have surfaced.
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

// TestNativeStreamFinishLineSteerStillPauses pins the finish-line race fix: a
// steer that the stream watcher consumes JUST as Stream returns CLEANLY (err==nil)
// must NOT be silently dropped — the loop carries it into the post-completion
// CV-T01 pause, so the full proposed action is HELD and the feedback folded.
func TestNativeStreamFinishLineSteerStillPauses(t *testing.T) {
	f := &fakeStreamer{
		entered: make(chan int, 4),
		release: make(chan struct{}),
		steps: []streamStep{
			// step 0: streams a delta, signals entered, then parks IGNORING ctx so it
			// returns CLEANLY even after the steer cancels streamCtx. Its full carries a
			// `run` proposal the CV-T01 pause must HOLD (never execute).
			{
				deltas:     []string{"deciding"},
				blockClean: true,
				full: model.Response{
					Content:    []model.Block{textBlock("deciding"), toolUse("u1", "run", map[string]string{"cmd": "echo finish-line"})},
					StopReason: "tool_use",
				},
			},
			// step 1: after the steer fold, the model reconsiders and finishes.
			{
				full: model.Response{
					Content:    []model.Block{toolUse("u2", "finish", map[string]string{"summary": "done"})},
					StopReason: "tool_use",
				},
			},
		},
	}
	ib := inbox.New(nil, "conv")
	em := &recordingEmitter{}
	box := &recordingBox{}
	n := &Native{Model: f, Box: box, Verifier: okVerifier{}, Inbox: ib, Emitter: em, MaxSteps: 5}

	done := make(chan error, 1)
	go func() { _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); done <- err }()

	<-f.entered // step 0 parked (blockClean), about to return cleanly
	// Push a steer; the watcher (already blocked on the steer channel) consumes it and
	// cancels streamCtx with ErrSteer, while the steered MESSAGE stays queued in the
	// inbox (Steer mode pushes both). The brief sleep biases the watcher to win the
	// race and consume the SIGNAL first (exercising the steerFired finish-line path);
	// but the assertions below hold EITHER way — if Stream instead returns before the
	// watcher acts, the still-pending token is caught by the post-completion
	// steerPending check. The decisive property is that a finish-line steer is never
	// silently dropped, regardless of which goroutine wins.
	ib.Push(userText("hold on — change of plan"), inbox.Steer)
	time.Sleep(20 * time.Millisecond)
	close(f.release) // step 0 returns full,nil; step 1 (no block) also uses this closed chan

	if err := <-done; err != nil {
		t.Fatalf("Run errored on a finish-line steer: %v", err)
	}
	hist := f.lastHistory()
	if !hasUserText(hist, "hold on — change of plan") {
		t.Error("finish-line steer was dropped — not folded as a user turn")
	}
	if !hasToolResult(hist, "Paused before this ran") {
		t.Error("finish-line steer did not HOLD the proposed action (CV-T01 pause skipped)")
	}
	for _, c := range box.execed {
		if strings.Contains(c, "finish-line") {
			t.Errorf("held command executed despite the finish-line steer: %q", c)
		}
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

// TestNativeStreamShutdownAbortsCleanly asserts that a parent-ctx cancel
// (shutdown/deadline) while a Stream is in flight aborts cleanly — classified as a
// SHUTDOWN, never a steer (which would continue) and never a fault (which would
// surface a transport error). Run returns no error and an "interrupted" Result.
func TestNativeStreamShutdownAbortsCleanly(t *testing.T) {
	f := &fakeStreamer{
		entered: make(chan int, 2),
		release: make(chan struct{}),
		steps: []streamStep{
			{
				deltas:   []string{"streaming "},
				honorCtx: true,
				partial:  model.Response{Content: []model.Block{textBlock("streaming ")}},
			},
		},
	}
	ib := inbox.New(nil, "conv")
	em := &recordingEmitter{}
	n := &Native{Model: f, Box: &recordingBox{}, Verifier: okVerifier{}, Inbox: ib, Emitter: em, MaxSteps: 5}

	ctx, cancel := context.WithCancel(context.Background())
	type out struct {
		res Result
		err error
	}
	dch := make(chan out, 1)
	go func() { r, e := n.Run(ctx, Task{ID: "t", Goal: "x"}); dch <- out{r, e} }()

	<-f.entered // step 0 parked inside Stream
	cancel()    // shutdown the parent ctx mid-stream

	select {
	case o := <-dch:
		if o.err != nil {
			t.Fatalf("parent-cancel must return a clean interrupted Result, not an error: %v", o.err)
		}
		if !strings.Contains(o.res.Summary, "interrupted") {
			t.Errorf("expected an interrupted summary, got %q", o.res.Summary)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not abort on parent-ctx cancel during a stream (shutdown not honored)")
	}
}

// TestNativeStreamNoWatcherGoroutineLeak asserts that across a multi-step
// streaming loop (a watcher spawns each iteration when an Inbox is wired) every
// watcher is torn down and the goroutine count returns to baseline — no leak.
func TestNativeStreamNoWatcherGoroutineLeak(t *testing.T) {
	base := waitGoroutines(runtime.NumGoroutine())
	f := &fakeStreamer{steps: []streamStep{
		{deltas: []string{"a"}, full: model.Response{Content: []model.Block{textBlock("a"), toolUse("u1", "run", map[string]string{"cmd": "echo a"})}, StopReason: "tool_use"}},
		{deltas: []string{"b"}, full: model.Response{Content: []model.Block{textBlock("b"), toolUse("u2", "run", map[string]string{"cmd": "echo b"})}, StopReason: "tool_use"}},
		{deltas: []string{"c"}, full: model.Response{Content: []model.Block{textBlock("c"), toolUse("u3", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"}},
	}}
	ib := inbox.New(nil, "conv")
	em := &recordingEmitter{}
	n := &Native{Model: f, Box: &recordingBox{}, Verifier: okVerifier{}, Inbox: ib, Emitter: em, MaxSteps: 10}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if g := waitGoroutines(base); g > base {
		t.Errorf("stream watcher goroutine leaked: baseline %d, after %d", base, g)
	}
}

// TestNativeStreamerWithNilEmitterUsesComplete asserts the gate: a Streamer
// provider with NO Emitter wired takes the Complete (non-streaming) path —
// byte-identical to the original loop. No KindToken events can be emitted (there
// is no sink), and the loop still drives to a normal finish.
func TestNativeStreamerWithNilEmitterUsesComplete(t *testing.T) {
	f := &fakeStreamer{steps: []streamStep{
		{
			// Deltas would only be forwarded on the Stream path; the Complete path
			// returns full directly, so these are never seen.
			deltas: []string{"never ", "streamed"},
			full:   model.Response{Content: []model.Block{toolUse("u1", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
		},
	}}
	// No Emitter → Complete path even though f is a Streamer.
	n := &Native{Model: f, Box: &recordingBox{}, Verifier: okVerifier{}, MaxSteps: 5}
	res, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.SelfClaimed {
		t.Error("streamer-with-nil-Emitter loop should finish via the Complete path")
	}
}

// nonStreamer is a plain model.Provider with NO Stream method, used to assert the
// loop falls back to Complete even when an Emitter is wired (no streaming token
// events) — byte-identical to the existing emit-only behavior.
type nonStreamer struct {
	responses []model.Response
	i         int
}

func (s *nonStreamer) Model() string { return "non-streamer" }
func (s *nonStreamer) Complete(context.Context, string, []model.Message, []model.Tool, int) (model.Response, error) {
	if s.i >= len(s.responses) {
		return model.Response{StopReason: "end_turn"}, nil
	}
	r := s.responses[s.i]
	s.i++
	return r, nil
}

// TestNativeNonStreamerWithEmitterUsesComplete asserts the other half of the gate:
// an Emitter wired but a provider that does NOT implement Streamer takes the
// Complete path — no KindToken events, the existing KindIntent reasoning surface
// still works, and the loop drives to a normal finish.
func TestNativeNonStreamerWithEmitterUsesComplete(t *testing.T) {
	m := &nonStreamer{responses: []model.Response{
		{Content: []model.Block{textBlock("thinking"), toolUse("u1", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	em := &recordingEmitter{}
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, Emitter: em, MaxSteps: 5}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// No token events can be produced by a non-streamer.
	if toks := tokenTexts(em); len(toks) != 0 {
		t.Errorf("non-streamer produced KindToken events: %v", toks)
	}
	// The existing per-step intent surface still fires.
	var sawIntent bool
	for _, k := range em.kinds() {
		if k == emit.KindIntent {
			sawIntent = true
		}
	}
	if !sawIntent {
		t.Error("non-streamer+Emitter path lost the KindIntent reasoning surface")
	}
}

// lastHistory returns the message history offered on the most recent Stream (or
// Complete) call, so a test can assert a folded/preserved turn appears.
func (f *fakeStreamer) lastHistory() []model.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastMsgs
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
