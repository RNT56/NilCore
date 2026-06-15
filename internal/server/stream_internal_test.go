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
