package backend

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"nilcore/internal/emit"
	"nilcore/internal/inbox"
	"nilcore/internal/model"
	"nilcore/internal/sandbox"
)

// --- C1-T03: interruptible inbox + emit seam --------------------------------
//
// These tests exercise the QUEUE/STEER seam added to the native loop. The
// decisive properties under test (design §4, risks #1/#2/#3/#6):
//   - nil Inbox AND nil Emitter ⇒ byte-identical: no extra goroutine, no Drain;
//   - a STEER cancels an in-flight (blocking) Model.Complete and folds the steer
//     text in at the next boundary — never misread as a fault;
//   - a QUEUE message is appended at the next loop boundary;
//   - a parent-ctx cancel (shutdown/deadline) aborts cleanly, never a steer;
//   - a STEER never cancels an in-flight Box.Exec (tools run to completion);
//   - no watcher goroutine leaks across iterations.
// All concurrency uses real goroutines + channels and runs under `go test -race`.

// blockingModel blocks each Complete on a release channel until the test (or a
// steer/shutdown cancelling ctx) lets it proceed. It records the message history
// offered on each call so a test can assert a folded user turn appears. It is the
// model that "blocks until steered" the acceptance criteria call for.
type blockingModel struct {
	mu        sync.Mutex
	calls     int
	lastMsgs  []model.Message
	release   chan struct{}    // closed/sent-to to let a parked Complete return
	responses []model.Response // returned in order once released
	entered   chan int         // signals the test that Complete N has parked
}

func (m *blockingModel) Model() string { return "fake" }

func (m *blockingModel) Complete(ctx context.Context, _ string, msgs []model.Message, _ []model.Tool, _ int) (model.Response, error) {
	m.mu.Lock()
	n := m.calls
	m.calls++
	m.lastMsgs = msgs
	m.mu.Unlock()

	if m.entered != nil {
		// Non-blocking notify so a test can wait until this call has actually
		// parked inside Complete before pushing a steer (deterministic, race-safe).
		select {
		case m.entered <- n:
		default:
		}
	}

	// Park until released OR ctx is cancelled (a steer/shutdown). Honoring ctx is
	// what a real provider does and what makes the steer observable.
	select {
	case <-ctx.Done():
		return model.Response{}, ctx.Err()
	case <-m.release:
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if n < len(m.responses) {
		return m.responses[n], nil
	}
	return model.Response{StopReason: "end_turn"}, nil
}

func (m *blockingModel) history() []model.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastMsgs
}

// recordingEmitter captures every surfaced event for assertions.
type recordingEmitter struct {
	mu     sync.Mutex
	events []emit.Event
}

func (r *recordingEmitter) Emit(e emit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingEmitter) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

// userText builds a principal user turn (un-Wrap'd, the trust line) the way a
// session's Turn would before pushing it into the inbox.
func userText(s string) model.Message {
	return model.Message{Role: "user", Content: []model.Block{{Type: "text", Text: s}}}
}

// hasUserText reports whether any user message in the history carries text.
func hasUserText(msgs []model.Message, text string) bool {
	for _, m := range msgs {
		if m.Role != "user" {
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

// waitGoroutines polls until the goroutine count settles at or below want, so a
// leak assertion is not flaky against a watcher that is mid-teardown.
func waitGoroutines(want int) int {
	for i := 0; i < 200; i++ {
		runtime.GC()
		if g := runtime.NumGoroutine(); g <= want {
			return g
		}
		time.Sleep(5 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}

// TestNativeNilInboxNoExtraGoroutine asserts the nil-seam is byte-identical in
// the way that matters for the parallel-agent rail: with Inbox==nil the loop
// spawns NO watcher goroutine (design adv #11, risk #2). We run a multi-step loop
// and assert the goroutine count returns to its pre-Run baseline.
func TestNativeNilInboxNoExtraGoroutine(t *testing.T) {
	base := waitGoroutines(runtime.NumGoroutine())
	m := &scriptModel{responses: []model.Response{
		{Content: []model.Block{toolUse("u1", "run", map[string]string{"cmd": "echo a"})}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u2", "run", map[string]string{"cmd": "echo b"})}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u3", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, MaxSteps: 10}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if g := waitGoroutines(base); g > base {
		t.Errorf("nil-Inbox loop leaked goroutines: baseline %d, after %d", base, g)
	}
}

// TestNativeSeedContinuesConversation asserts a non-nil Seed pre-loads prior
// history so the drive CONTINUES rather than restarts: the seeded turn precedes
// the new goal turn in the history the model sees.
func TestNativeSeedContinuesConversation(t *testing.T) {
	m := &toolCapturingModel{responses: []model.Response{
		{Content: []model.Block{toolUse("u1", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	seed := []model.Message{
		userText("earlier: build the parser"),
		{Role: "assistant", Content: []model.Block{{Type: "text", Text: "parser built"}}},
	}
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, Seed: seed, MaxSteps: 5}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "now add tests"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasUserText(m.lastMsgs, "earlier: build the parser") {
		t.Error("seed turn missing from history — drive restarted instead of continuing")
	}
	if !hasUserText(m.lastMsgs, "now add tests") {
		t.Error("new goal turn missing from history")
	}
	// Order: the seed turn must precede the goal turn.
	si, gi := -1, -1
	for i, msg := range m.lastMsgs {
		for _, b := range msg.Content {
			if strings.Contains(b.Text, "earlier: build the parser") {
				si = i
			}
			if strings.Contains(b.Text, "now add tests") {
				gi = i
			}
		}
	}
	if si < 0 || gi < 0 || si >= gi {
		t.Errorf("seed (idx %d) must precede goal (idx %d)", si, gi)
	}
}

// TestNativeQueuedMessageFoldedAtBoundary asserts a Queue-pushed message is
// drained and appended as a user turn at the next loop boundary (design §4.3).
func TestNativeQueuedMessageFoldedAtBoundary(t *testing.T) {
	box := &blockingTool{release: make(chan struct{})}
	close(box.release) // tool returns immediately for this test
	m := &blockingModel{
		release: make(chan struct{}),
		entered: make(chan int, 4),
		responses: []model.Response{
			{Content: []model.Block{toolUse("u1", "run", map[string]string{"cmd": "echo a"})}, StopReason: "tool_use"},
			{Content: []model.Block{toolUse("u2", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
		},
	}
	ib := inbox.New(nil, "conv")
	n := &Native{Model: m, Box: box, Verifier: okVerifier{}, Inbox: ib, MaxSteps: 5}

	done := make(chan error, 1)
	go func() { _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); done <- err }()

	<-m.entered // step 0 parked
	// Queue a message; it must NOT cancel the model call, only fold at the next
	// boundary. Release step 0 so the loop proceeds to step 1's Drain.
	ib.Push(userText("also handle errors"), inbox.Queue)
	m.release <- struct{}{} // release step 0
	<-m.entered             // step 1 parked → its Drain already ran
	if !hasUserText(m.history(), "also handle errors") {
		t.Error("queued message not folded as a user turn at the next boundary")
	}
	m.release <- struct{}{} // release step 1 (finish)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestNativeSteerCancelsInFlightModelCall is the core acceptance test: a steer
// cancels a BLOCKING Model.Complete now (not at the next boundary), the cancel is
// reclassified as a steer (NOT a fault → Run does not error), and the steer text
// is folded in as the next user turn. A steer_ack is emitted.
func TestNativeSteerCancelsInFlightModelCall(t *testing.T) {
	m := &blockingModel{
		release: make(chan struct{}),
		entered: make(chan int, 4),
		responses: []model.Response{
			// step 0 is steered (cancelled) before it can return; on the SECOND
			// call after the steer fold the model finishes.
			{Content: []model.Block{toolUse("u1", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
			{Content: []model.Block{toolUse("u2", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
		},
	}
	ib := inbox.New(nil, "conv")
	em := &recordingEmitter{}
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, Inbox: ib, Emitter: em, MaxSteps: 5}

	done := make(chan error, 1)
	go func() { _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); done <- err }()

	<-m.entered // step 0 parked inside Complete
	ib.Push(userText("use ./service not ./cmd"), inbox.Steer)
	// The steer cancels step 0's Complete; the loop reclassifies it as a steer and
	// continues to step 1, which Drains the steer text and then parks.
	<-m.entered // step 1 parked → its Drain folded the steer message
	if !hasUserText(m.history(), "use ./service not ./cmd") {
		t.Error("steer text not folded as a user turn after the cancel")
	}
	m.release <- struct{}{} // release step 1 (finish)
	if err := <-done; err != nil {
		t.Fatalf("Run errored on a steer (steer misread as fault): %v", err)
	}
	// A steer_ack must have been surfaced.
	var sawAck bool
	for _, k := range em.kinds() {
		if k == emit.KindSteerAck {
			sawAck = true
		}
	}
	if !sawAck {
		t.Error("no steer_ack emitted after a steer fold")
	}
}

// TestNativeParentCancelAbortsNotSteer asserts a parent-ctx cancel (shutdown /
// deadline) while a Model.Complete is in flight aborts cleanly — classified as a
// SHUTDOWN, never a steer (which would continue past the termination rail) and
// never a fault (which would surface a transport error). Run returns no error and
// an "interrupted" Result (design §4.3, risk #3).
func TestNativeParentCancelAbortsNotSteer(t *testing.T) {
	m := &blockingModel{release: make(chan struct{}), entered: make(chan int, 2)}
	ib := inbox.New(nil, "conv")
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, Inbox: ib, MaxSteps: 5}

	ctx, cancel := context.WithCancel(context.Background())
	type out struct {
		res Result
		err error
	}
	done := make(chan out, 1)
	go func() { r, e := n.Run(ctx, Task{ID: "t", Goal: "x"}); done <- out{r, e} }()

	<-m.entered // step 0 parked
	cancel()    // shutdown the parent ctx while Complete is in flight

	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("parent-cancel must return a clean interrupted Result, not an error: %v", o.err)
		}
		if !strings.Contains(o.res.Summary, "interrupted") {
			t.Errorf("expected an interrupted summary, got %q", o.res.Summary)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not abort on parent-ctx cancel (shutdown not honored)")
	}
}

// blockingTool is a sandbox whose Exec blocks on release, so a test can hold a
// tool in flight while it pushes a steer and assert the steer does NOT cancel it.
type blockingTool struct {
	mu       sync.Mutex
	started  chan struct{}
	release  chan struct{}
	finished bool
	gotCtx   context.Context
}

func (b *blockingTool) Exec(ctx context.Context, _ string) (sandbox.Result, error) {
	if b.started != nil {
		select {
		case b.started <- struct{}{}:
		default:
		}
	}
	b.mu.Lock()
	b.gotCtx = ctx
	b.mu.Unlock()
	// Block until released. If the steer were (wrongly) threaded into the tool
	// ctx, this select would observe ctx.Done before release and report it.
	select {
	case <-ctx.Done():
		return sandbox.Result{}, ctx.Err()
	case <-b.release:
	}
	b.mu.Lock()
	b.finished = true
	b.mu.Unlock()
	return sandbox.Result{Stdout: "ok"}, nil
}

func (b *blockingTool) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return b.Exec(ctx, cmd)
}
func (b *blockingTool) Workdir() string { return "/work" }

func (b *blockingTool) didFinish() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.finished
}

// TestNativeSteerDoesNotCancelInFlightTool is the decisive scope test (risk #1):
// the iter-ctx wraps ONLY Model.Complete, never Box.Exec — so a steer that lands
// while a tool is running does NOT cancel the tool; the tool runs to completion
// and the steer applies at the NEXT boundary. A SIGKILL of a real container
// mid-write would tear the RW-bind-mounted /work, so this property is load-bearing.
func TestNativeSteerDoesNotCancelInFlightTool(t *testing.T) {
	box := &blockingTool{started: make(chan struct{}, 1), release: make(chan struct{})}
	m := &blockingModel{
		release: make(chan struct{}),
		entered: make(chan int, 4),
		responses: []model.Response{
			{Content: []model.Block{toolUse("u1", "run", map[string]string{"cmd": "go build ./..."})}, StopReason: "tool_use"},
			{Content: []model.Block{toolUse("u2", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
		},
	}
	ib := inbox.New(nil, "conv")
	n := &Native{Model: m, Box: box, Verifier: okVerifier{}, Inbox: ib, MaxSteps: 5}

	done := make(chan error, 1)
	go func() { _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); done <- err }()

	<-m.entered             // step 0 parked
	m.release <- struct{}{} // release step 0 → loop dispatches the `run` tool
	<-box.started           // tool now in flight (Box.Exec blocked)

	// Steer NOW, while the tool runs. The tool must NOT be cancelled: it carries
	// the TASK ctx, not the iter-ctx. Give the steer a moment to be observed.
	ib.Push(userText("wrong path"), inbox.Steer)
	time.Sleep(50 * time.Millisecond)
	if box.didFinish() {
		t.Fatal("tool finished before release — test setup wrong")
	}
	// Release the tool: it must complete normally (proving it was never cancelled).
	close(box.release)
	if !waitTool(box) {
		t.Fatal("in-flight tool was cancelled by a steer — iter-ctx leaked into Box.Exec")
	}

	<-m.entered             // step 1 parked (the steer folded in at this boundary)
	m.release <- struct{}{} // release step 1 (finish)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if box.gotCtx == nil || errors.Is(box.gotCtx.Err(), context.Canceled) {
		t.Error("tool received a cancelled (iter) ctx; it must receive the live task ctx")
	}
}

func waitTool(b *blockingTool) bool {
	for i := 0; i < 200; i++ {
		if b.didFinish() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestNativeNoWatcherGoroutineLeakWithInbox asserts that across a multi-step loop
// WITH an Inbox wired (so a watcher spawns each iteration), every watcher is torn
// down (cancel(nil); <-watcher) and the goroutine count returns to baseline — no
// per-iteration leak (risk #6).
func TestNativeNoWatcherGoroutineLeakWithInbox(t *testing.T) {
	base := waitGoroutines(runtime.NumGoroutine())
	m := &scriptModel{responses: []model.Response{
		{Content: []model.Block{toolUse("u1", "run", map[string]string{"cmd": "echo a"})}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u2", "run", map[string]string{"cmd": "echo b"})}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u3", "run", map[string]string{"cmd": "echo c"})}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u4", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	ib := inbox.New(nil, "conv")
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, Inbox: ib, MaxSteps: 10}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if g := waitGoroutines(base); g > base {
		t.Errorf("watcher goroutine leaked: baseline %d, after %d", base, g)
	}
}

// TestNativeNilEmitterByteIdentical asserts the Emitter gate: a nil Emitter is a
// safe no-op (the loop nil-checks before every Emit), so wiring only an Inbox (no
// Emitter) runs clean — the emit path never panics on a nil sink.
func TestNativeNilEmitterByteIdentical(t *testing.T) {
	m := &scriptModel{responses: []model.Response{
		{Content: []model.Block{{Type: "text", Text: "thinking"}, toolUse("u1", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	ib := inbox.New(nil, "conv")
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, Inbox: ib, MaxSteps: 5}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); err != nil {
		t.Fatalf("Run with nil Emitter: %v", err)
	}
}

// stdoutMarkerBox is a sandbox whose Exec returns a distinctive marker in its
// stdout/stderr, so a test can assert that raw tool OUTPUT is NEVER surfaced as an
// action-intent line (adv #8): the loop surfaces a harness-authored intent (the
// command it is ABOUT to run), not laundered output.
type stdoutMarkerBox struct{ marker string }

func (b *stdoutMarkerBox) Exec(context.Context, string) (sandbox.Result, error) {
	return sandbox.Result{Stdout: b.marker, Stderr: b.marker, ExitCode: 0}, nil
}
func (b *stdoutMarkerBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return b.Exec(ctx, cmd)
}
func (b *stdoutMarkerBox) Workdir() string { return "/work" }

// TestNativeEmitsActionIntentBeforeRun is the C2-T04 acceptance test: with an
// Emitter wired, the native loop surfaces an action-intent (KindTool) carrying the
// command it is ABOUT to run, BEFORE Box.Exec dispatches it — so a watching
// principal sees the action coming and can steer on it. The intent must contain the
// command (the model's own input) and MUST NOT contain raw tool output (adv #8).
func TestNativeEmitsActionIntentBeforeRun(t *testing.T) {
	const marker = "RAW_TOOL_OUTPUT_SHOULD_NEVER_SURFACE"
	m := &scriptModel{responses: []model.Response{
		{Content: []model.Block{toolUse("u1", "run", map[string]string{"cmd": "go test ./..."})}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u2", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	em := &recordingEmitter{}
	n := &Native{Model: m, Box: &stdoutMarkerBox{marker: marker}, Verifier: okVerifier{}, Emitter: em, MaxSteps: 5}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	em.mu.Lock()
	defer em.mu.Unlock()
	var sawRunIntent bool
	for _, e := range em.events {
		// No surfaced event of any kind may carry raw tool output (adv #8).
		if strings.Contains(e.Text, marker) {
			t.Errorf("raw tool output leaked into a surfaced %s event: %q", e.Kind, e.Text)
		}
		if e.Kind == emit.KindTool && strings.Contains(e.Text, "about to run") && strings.Contains(e.Text, "go test ./...") {
			sawRunIntent = true
		}
	}
	if !sawRunIntent {
		t.Error("no KindTool action-intent surfaced for the command before it ran")
	}
}

// TestNativeEmitsFinishIntentBeforeVerify asserts the loop surfaces an action
// intent before the verifier runs on a finish (the verifier — not the model —
// decides done, I2; surfacing it lets the principal steer before the verdict).
func TestNativeEmitsFinishIntentBeforeVerify(t *testing.T) {
	m := &scriptModel{responses: []model.Response{
		{Content: []model.Block{toolUse("u1", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	em := &recordingEmitter{}
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, Emitter: em, MaxSteps: 5}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawFinishIntent bool
	em.mu.Lock()
	for _, e := range em.events {
		if e.Kind == emit.KindTool && strings.Contains(e.Text, "verifier will judge") {
			sawFinishIntent = true
		}
	}
	em.mu.Unlock()
	if !sawFinishIntent {
		t.Error("no action-intent surfaced before the verifier judged a finish")
	}
}

// TestNativeNilEmitterNoActionIntent asserts the action-intent path is fully gated
// on a nil Emitter — a nil sink runs the loop clean (byte-identical), never
// panicking on the new emit calls (the existing nil-path tests cover behavior; this
// pins the new run/finish/tool intent calls specifically).
func TestNativeNilEmitterNoActionIntent(t *testing.T) {
	m := &scriptModel{responses: []model.Response{
		{Content: []model.Block{toolUse("u1", "run", map[string]string{"cmd": "echo a"})}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u2", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, MaxSteps: 5}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); err != nil {
		t.Fatalf("Run with nil Emitter: %v", err)
	}
}

// TestNativeEmitsPerStepReasoning asserts a wired Emitter receives one intent
// event per text block of the assistant turn (the steer surface, §5.2).
func TestNativeEmitsPerStepReasoning(t *testing.T) {
	m := &scriptModel{responses: []model.Response{
		{Content: []model.Block{{Type: "text", Text: "I'll scaffold the handler"}, toolUse("u1", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	em := &recordingEmitter{}
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, Emitter: em, MaxSteps: 5}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawIntent bool
	for _, e := range em.events {
		if e.Kind == emit.KindIntent && strings.Contains(e.Text, "scaffold the handler") {
			sawIntent = true
		}
	}
	if !sawIntent {
		t.Error("no per-step intent surfaced for the assistant's text block")
	}
}
