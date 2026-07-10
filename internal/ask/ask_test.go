package ask

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"nilcore/internal/backend"
	"nilcore/internal/emit"
)

// chanEmitter signals each rendered question on a buffered channel so a test can
// synchronize with the collection loop without sleeping.
type chanEmitter struct{ ch chan emit.Event }

func (e *chanEmitter) Emit(ev emit.Event) {
	select {
	case e.ch <- ev:
	default:
	}
}

// TestEmitQuestionStructured asserts the question is surfaced with BOTH the plain
// Text (the fallback every surface can render) and a populated *emit.AskPrompt (what a
// widget surface renders natively) — the foundation for the per-surface native UI.
func TestEmitQuestionStructured(t *testing.T) {
	em := &chanEmitter{ch: make(chan emit.Event, 4)}
	b := New(em)
	qs := []backend.AskQuestion{{
		Prompt:      "which db?",
		Choices:     []backend.AskChoice{{Label: "Postgres", Detail: "managed"}, {Label: "SQLite"}},
		MultiSelect: true,
	}}
	go func() { _, _ = b.Ask(context.Background(), qs) }()
	ev := <-em.ch
	if ev.Kind != emit.KindAsk {
		t.Fatalf("kind = %q, want ask", ev.Kind)
	}
	if ev.Text == "" {
		t.Fatal("plain Text fallback must still be populated")
	}
	if ev.Ask == nil {
		t.Fatal("structured Ask payload must be populated")
	}
	a := ev.Ask
	if a.Index != 1 || a.Total != 1 || a.Question != "which db?" || !a.MultiSelect {
		t.Fatalf("payload header wrong: %+v", *a)
	}
	if len(a.Choices) != 2 || a.Choices[0].Label != "Postgres" || a.Choices[0].Detail != "managed" || a.Choices[1].Label != "SQLite" {
		t.Fatalf("payload choices wrong: %+v", a.Choices)
	}
	_ = b.Resolve("1")
}

// TestResolveReply is the normative resolution table (§3.4): single-select takes a
// bare integer else free-form verbatim; multi-select splits on ';'; an out-of-range
// or non-integer token makes the whole line free-form; empty declines.
func TestResolveReply(t *testing.T) {
	choices := []backend.AskChoice{{Label: "Alpha"}, {Label: "Beta"}, {Label: "Gamma"}}
	single := backend.AskQuestion{Prompt: "q", Choices: choices}
	multi := backend.AskQuestion{Prompt: "q", Choices: choices, MultiSelect: true}
	free := backend.AskQuestion{Prompt: "q"}

	tests := []struct {
		name string
		q    backend.AskQuestion
		line string
		want backend.AskAnswer
	}{
		{"single bare int", single, "2", backend.AskAnswer{Selected: []string{"Beta"}}},
		{"single out of range -> free", single, "9", backend.AskAnswer{Custom: "9"}},
		{"single multi-number -> free", single, "1,3", backend.AskAnswer{Custom: "1,3"}},
		{"single qualifier -> free", single, "2 but staging", backend.AskAnswer{Custom: "2 but staging"}},
		{"single prose -> free", single, "do something else", backend.AskAnswer{Custom: "do something else"}},
		{"single empty -> declined", single, "  ", backend.AskAnswer{}},
		{"multi two indices", multi, "1,3", backend.AskAnswer{Selected: []string{"Alpha", "Gamma"}}},
		{"multi indices + custom", multi, "1,3 ; only on staging", backend.AskAnswer{Selected: []string{"Alpha", "Gamma"}, Custom: "only on staging"}},
		{"multi dupes deduped, menu order", multi, "3,1,1", backend.AskAnswer{Selected: []string{"Alpha", "Gamma"}}},
		{"multi out-of-range token -> free", multi, "1 9", backend.AskAnswer{Custom: "1 9"}},
		{"multi non-integer -> free", multi, "alpha", backend.AskAnswer{Custom: "alpha"}},
		{"multi single index", multi, "2", backend.AskAnswer{Selected: []string{"Beta"}}},
		{"free-form pure", free, "use the lib", backend.AskAnswer{Custom: "use the lib"}},
		{"free-form bare int stays text", free, "2", backend.AskAnswer{Custom: "2"}},
		{"free-form empty declined", free, "", backend.AskAnswer{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveReply(tt.q, tt.line)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("resolveReply(%q) = %+v, want %+v", tt.line, got, tt.want)
			}
		})
	}
}

// TestBoxBatchRoundtrip drives a 3-question batch: each emitted question is answered
// via Resolve, and the returned answers match (and a '!'-leading line is a free-form
// answer, never a steer).
func TestBoxBatchRoundtrip(t *testing.T) {
	em := &chanEmitter{ch: make(chan emit.Event, 16)}
	b := New(em)
	qs := []backend.AskQuestion{
		{Prompt: "pick one", Choices: []backend.AskChoice{{Label: "X"}, {Label: "Y"}}},
		{Prompt: "free"},
		{Prompt: "multi", Choices: []backend.AskChoice{{Label: "A"}, {Label: "B"}}, MultiSelect: true},
	}
	replies := []string{"2", "!do it in staging", "1,2"}
	type res struct {
		a   []backend.AskAnswer
		err error
	}
	done := make(chan res, 1)
	go func() {
		a, err := b.Ask(context.Background(), qs)
		done <- res{a, err}
	}()
	for i := range qs {
		<-em.ch // question i was rendered; the box is (about to be) waiting
		if !b.Resolve(replies[i]) {
			t.Fatalf("Resolve(%q) returned false", replies[i])
		}
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("Ask err: %v", got.err)
	}
	want := []backend.AskAnswer{
		{Selected: []string{"Y"}},
		{Custom: "!do it in staging"},
		{Selected: []string{"A", "B"}},
	}
	if !reflect.DeepEqual(got.a, want) {
		t.Fatalf("answers = %+v, want %+v", got.a, want)
	}
	if b.Pending() {
		t.Fatal("box still pending after batch")
	}
}

// TestBoxTimeout returns partial answers + ErrAskTimeout when the operator goes
// silent mid-batch (the backstop bounds absence; partial answers are never dropped).
func TestBoxTimeout(t *testing.T) {
	em := &chanEmitter{ch: make(chan emit.Event, 16)}
	b := New(em)
	b.backstop = 30 * time.Millisecond
	qs := []backend.AskQuestion{{Prompt: "a"}, {Prompt: "b"}}
	type res struct {
		a   []backend.AskAnswer
		err error
	}
	done := make(chan res, 1)
	go func() {
		a, err := b.Ask(context.Background(), qs)
		done <- res{a, err}
	}()
	<-em.ch // first question rendered
	if !b.Resolve("first answer") {
		t.Fatal("Resolve failed")
	}
	<-em.ch // second question rendered; now go silent → backstop fires
	got := <-done
	if !errors.Is(got.err, backend.ErrAskTimeout) {
		t.Fatalf("err = %v, want ErrAskTimeout", got.err)
	}
	if len(got.a) != 1 || got.a[0].Custom != "first answer" {
		t.Fatalf("partial answers = %+v, want the one collected answer", got.a)
	}
}

// TestBoxCancel returns ctx.Err and partial answers when the drive is cancelled while
// parked (the Cancel/shutdown path).
func TestBoxCancel(t *testing.T) {
	em := &chanEmitter{ch: make(chan emit.Event, 16)}
	b := New(em)
	ctx, cancel := context.WithCancel(context.Background())
	qs := []backend.AskQuestion{{Prompt: "a"}}
	done := make(chan error, 1)
	go func() {
		_, err := b.Ask(ctx, qs)
		done <- err
	}()
	<-em.ch
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if b.Pending() {
		t.Fatal("box still pending after cancel")
	}
}

// TestBoxSingleFlight rejects a second concurrent batch.
func TestBoxSingleFlight(t *testing.T) {
	em := &chanEmitter{ch: make(chan emit.Event, 16)}
	b := New(em)
	qs := []backend.AskQuestion{{Prompt: "a"}}
	go func() { _, _ = b.Ask(context.Background(), qs) }()
	<-em.ch // first batch is now collecting
	if _, err := b.Ask(context.Background(), qs); err == nil {
		t.Fatal("second concurrent Ask should error (single-flight)")
	}
}

// TestReprompt re-prompts once on a first empty reply, then declines on a second.
func TestReprompt(t *testing.T) {
	em := &chanEmitter{ch: make(chan emit.Event, 16)}
	b := New(em)
	qs := []backend.AskQuestion{{Prompt: "a"}}
	done := make(chan []backend.AskAnswer, 1)
	go func() {
		a, _ := b.Ask(context.Background(), qs)
		done <- a
	}()
	<-em.ch // question rendered
	if !b.Resolve("") {
		t.Fatal("first empty resolve failed")
	}
	<-em.ch // re-prompt rendered
	if !b.Resolve("") {
		t.Fatal("second empty resolve failed")
	}
	a := <-done
	if len(a) != 1 || len(a[0].Selected) != 0 || a[0].Custom != "" {
		t.Fatalf("want one declined answer, got %+v", a)
	}
}

// TestResolveAfterBatchEndFallsThrough locks the atomic check-and-send: once a batch has
// fully collected and Ask returned (its defer flipped pending=false under the box lock), a
// late Resolve must return false so Session.Turn falls through to the follow-up path instead
// of stranding the line in the just-drained cap-1 reply buffer (and reporting a bogus true).
func TestResolveAfterBatchEndFallsThrough(t *testing.T) {
	em := &chanEmitter{ch: make(chan emit.Event, 4)}
	b := New(em)
	done := make(chan struct{})
	go func() {
		_, _ = b.Ask(context.Background(), []backend.AskQuestion{{Prompt: "q"}})
		close(done)
	}()
	<-em.ch // the box is (about to be) waiting on the one question
	if !b.Resolve("answer") {
		t.Fatal("Resolve during an active batch must deliver the answer")
	}
	<-done // Ask fully returned ⇒ pending is false under the lock
	if b.Pending() {
		t.Fatal("Pending must be false after Ask returned")
	}
	if b.Resolve("a late extra line") {
		t.Fatal("a Resolve after the batch ended must return false (fall through), never strand the line")
	}
}

// TestResolveConcurrentWithBatchEnd stresses Resolve racing the batch teardown under the
// race detector: many rounds, each answering a one-question batch while a SECOND goroutine
// fires an extra Resolve around the batch's end. It asserts no deadlock/panic and that Ask's
// answer is always exactly one of the two sent lines — never corrupted, never empty (which
// would mean the delivered reply was stranded by the teardown). The atomic check-and-send
// keeps the single-flight rendezvous coherent under the race.
func TestResolveConcurrentWithBatchEnd(t *testing.T) {
	for round := 0; round < 200; round++ {
		em := &chanEmitter{ch: make(chan emit.Event, 4)}
		b := New(em)
		got := make(chan string, 1)
		go func() {
			a, _ := b.Ask(context.Background(), []backend.AskQuestion{{Prompt: "q"}})
			if len(a) == 1 {
				got <- a[0].Custom
			} else {
				got <- "<none>"
			}
		}()
		<-em.ch // box about to wait; both Resolves now race the consume + teardown
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); b.Resolve("first") }()
		go func() { defer wg.Done(); b.Resolve("second") }()
		wg.Wait()
		if ans := <-got; ans != "first" && ans != "second" {
			t.Fatalf("round %d: answer %q is neither sent line — the rendezvous was corrupted", round, ans)
		}
	}
}
