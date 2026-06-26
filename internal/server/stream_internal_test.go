package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"nilcore/internal/channel"
	"nilcore/internal/emit"
)

// draftFake is a Channel that also implements channel.DraftStreamer, recording
// every StreamDraft / FinalizeRich / Update call for assertions.
type draftFake struct {
	mu      sync.Mutex
	drafts  []string
	finals  []string
	updates []string
}

func (f *draftFake) Receive(context.Context) (channel.TaskRequest, error) {
	return channel.TaskRequest{}, context.Canceled
}
func (f *draftFake) Update(_ context.Context, _ string, msg string) error {
	f.mu.Lock()
	f.updates = append(f.updates, msg)
	f.mu.Unlock()
	return nil
}
func (f *draftFake) Ask(context.Context, string, string) (bool, error) { return true, nil }
func (f *draftFake) StreamDraft(_ context.Context, _ string, _ int64, text string) error {
	f.mu.Lock()
	f.drafts = append(f.drafts, text)
	f.mu.Unlock()
	return nil
}
func (f *draftFake) FinalizeRich(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	f.finals = append(f.finals, text)
	f.mu.Unlock()
	return nil
}

func (f *draftFake) saw(get func(*draftFake) []string, want string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range get(f) {
		if s == want {
			return true
		}
	}
	return false
}

func waitSaw(t *testing.T, f *draftFake, get func(*draftFake) []string, want, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.saw(get, want) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s never saw %q", what, want)
}

// TestChannelEmitterStreams proves a DraftStreamer transport gets live token
// streaming: tokens accumulate into one animated draft (not one message per
// token), and a framed event finalizes the streamed reasoning then emits its line.
func TestChannelEmitterStreams(t *testing.T) {
	fake := &draftFake{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &channelEmitter{ctx: ctx, ch: fake, thread: "t", throttle: 5 * time.Millisecond}

	e.Emit(emit.Event{Kind: emit.KindToken, Text: "Hel"})
	e.Emit(emit.Event{Kind: emit.KindToken, Text: "lo "})
	e.Emit(emit.Event{Kind: emit.KindToken, Text: "world"})
	// The growing buffer is pushed as one in-place draft (a later flush carries the
	// full accumulated text — never one draft per token's worth of spam).
	waitSaw(t, fake, func(f *draftFake) []string { return f.drafts }, "Hello world", "StreamDraft")

	// A framed event finalizes the streamed reasoning, then emits its own line.
	e.Emit(emit.Event{Kind: emit.KindTool, Text: "about to run: go test"})
	waitSaw(t, fake, func(f *draftFake) []string { return f.finals }, "Hello world", "FinalizeRich")
	waitSaw(t, fake, func(f *draftFake) []string { return f.updates }, "→ about to run: go test", "Update")

	cancel()
	e.wait() // clean join, no leak
}

// TestCoalesceKeepsFrames proves the bounded queue sheds the OLDEST TOKEN under
// backpressure but never a framed event (a dropped frame would lose a turn boundary
// and merge two turns into one draft). Order among survivors is preserved.
func TestCoalesceKeepsFrames(t *testing.T) {
	e := &channelEmitter{}
	e.buf = []emit.Event{
		{Kind: emit.KindToken, Text: "a"},
		{Kind: emit.KindTool, Text: "frame"},
		{Kind: emit.KindToken, Text: "b"},
	}
	e.coalesce()
	if len(e.buf) != 2 || e.buf[0].Kind != emit.KindTool || e.buf[1].Text != "b" {
		t.Fatalf("coalesce must drop the oldest token and keep the frame: %+v", e.buf)
	}
	// Pathological all-frames backlog: drop the oldest event so it stays bounded.
	e.buf = []emit.Event{{Kind: emit.KindTool, Text: "1"}, {Kind: emit.KindTool, Text: "2"}}
	e.coalesce()
	if len(e.buf) != 1 || e.buf[0].Text != "2" {
		t.Fatalf("all-frames coalesce must drop the oldest: %+v", e.buf)
	}
}

// TestStreamFinalizesOnStepChange proves that two turns never merge even when the
// framed boundary between them is missing: a token carrying a new step closes the
// prior turn as its own finalized message first. It also proves the shutdown path
// persists the in-flight reasoning on a detached context (the serve ctx is dead).
func TestStreamFinalizesOnStepChange(t *testing.T) {
	fake := &draftFake{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &channelEmitter{ctx: ctx, ch: fake, thread: "t", throttle: 5 * time.Millisecond}

	// Step 1's tokens, then step 2's tokens with NO framed boundary between them
	// (as if the boundary were coalesced away). The step change must finalize step 1.
	e.Emit(emit.Event{Kind: emit.KindToken, Step: 1, Text: "alpha"})
	e.Emit(emit.Event{Kind: emit.KindToken, Step: 2, Text: "beta"})
	waitSaw(t, fake, func(f *draftFake) []string { return f.finals }, "alpha", "step-change finalize")

	cancel() // shutdown with "beta" still in flight
	e.wait()
	if !fake.saw(func(f *draftFake) []string { return f.finals }, "beta") {
		t.Error("shutdown must persist the in-flight reasoning via a detached ctx")
	}
}

// A non-DraftStreamer channel keeps the plain per-line behaviour (no drafts).
func TestChannelEmitterPlainFallback(t *testing.T) {
	plain := &plainOnly{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &channelEmitter{ctx: ctx, ch: plain, thread: "t"}

	e.Emit(emit.Event{Kind: emit.KindIntent, Text: "thinking it through"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		plain.mu.Lock()
		ok := len(plain.updates) == 1 && plain.updates[0] == "thinking it through"
		plain.mu.Unlock()
		if ok {
			cancel()
			e.wait()
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("plain channel did not receive the rendered line via Update")
}

// plainOnly implements channel.Channel but NOT channel.DraftStreamer.
type plainOnly struct {
	mu      sync.Mutex
	updates []string
}

func (p *plainOnly) Receive(context.Context) (channel.TaskRequest, error) {
	return channel.TaskRequest{}, context.Canceled
}
func (p *plainOnly) Update(_ context.Context, _ string, msg string) error {
	p.mu.Lock()
	p.updates = append(p.updates, msg)
	p.mu.Unlock()
	return nil
}
func (p *plainOnly) Ask(context.Context, string, string) (bool, error) { return true, nil }

// choiceFake is a Channel that ALSO implements channel.ChoicePoster, recording every
// PostChoices call so a test can assert a structured ask renders as native buttons.
type choiceFake struct {
	draftFake
	pmu     sync.Mutex
	posted  []string
	choices [][]channel.AskChoice
	multi   []bool
}

func (f *choiceFake) PostChoices(_ context.Context, _, question string, choices []channel.AskChoice, m bool) error {
	f.pmu.Lock()
	f.posted = append(f.posted, question)
	f.choices = append(f.choices, choices)
	f.multi = append(f.multi, m)
	f.pmu.Unlock()
	return nil
}

// TestDeliverPostsChoices: a structured KindAsk renders as native buttons via
// ChoicePoster; a KindAsk WITHOUT a payload (a re-prompt) falls back to a plain Update;
// and a transport without ChoicePoster always falls back to Update (byte-identical).
func TestDeliverPostsChoices(t *testing.T) {
	f := &choiceFake{}
	e := &channelEmitter{ctx: context.Background(), ch: f, thread: "t"}
	e.deliver(emit.Event{Kind: emit.KindAsk, Text: "fallback", Ask: &emit.AskPrompt{
		Question: "Which database?", MultiSelect: true,
		Choices: []emit.AskChoice{{Label: "Postgres"}, {Label: "SQLite"}},
	}})
	if len(f.posted) != 1 || f.posted[0] != "Which database?" || !f.multi[0] {
		t.Fatalf("structured ask should PostChoices, got posted=%v multi=%v", f.posted, f.multi)
	}
	if len(f.choices[0]) != 2 || f.choices[0][0].Label != "Postgres" {
		t.Fatalf("choices not passed through: %+v", f.choices)
	}
	// A payload-less KindAsk must NOT post choices — it is a plain progress line.
	e.deliver(emit.Event{Kind: emit.KindAsk, Text: "(re-prompt)"})
	if len(f.posted) != 1 {
		t.Fatalf("payload-less ask must not post choices, posted=%v", f.posted)
	}

	// A transport WITHOUT ChoicePoster falls back to a plain Update (no panic).
	plain := &draftFake{}
	ep := &channelEmitter{ctx: context.Background(), ch: plain, thread: "t"}
	ep.deliver(emit.Event{Kind: emit.KindAsk, Text: "Q", Ask: &emit.AskPrompt{Question: "Q"}})
	if len(plain.updates) != 1 {
		t.Fatalf("non-poster transport should Update once, got %v", plain.updates)
	}
}

// TestCoalesceNeverDropsAsk: under a heavy framed backlog (no tokens to shed), coalesce
// drops other frames but NEVER the KindAsk question — a dropped ask would strand a
// parked drive for the full backstop.
func TestCoalesceNeverDropsAsk(t *testing.T) {
	e := &channelEmitter{ctx: context.Background(), ch: &draftFake{}, thread: "t"}
	for i := 0; i < 40; i++ {
		e.buf = append(e.buf, emit.Event{Kind: emit.KindTool, Text: "x"})
	}
	e.buf = append(e.buf, emit.Event{Kind: emit.KindAsk, Ask: &emit.AskPrompt{Question: "the question"}})
	for i := 0; i < 50; i++ {
		e.coalesce()
	}
	asks := 0
	for _, ev := range e.buf {
		if ev.Kind == emit.KindAsk {
			asks++
		}
	}
	if asks != 1 {
		t.Fatalf("KindAsk must survive coalesce, found %d in buf of %d", asks, len(e.buf))
	}
}
