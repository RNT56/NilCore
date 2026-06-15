package inbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/model"
)

// userMsg is a one-block user turn carrying text — the shape the loop folds in.
func userMsg(text string) model.Message {
	return model.Message{
		Role:    "user",
		Content: []model.Block{{Type: "text", Text: text}},
	}
}

func texts(msgs []model.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		var b strings.Builder
		for _, blk := range m.Content {
			b.WriteString(blk.Text)
		}
		out = append(out, b.String())
	}
	return out
}

// TestDrainReturnsThenClears: Drain hands back the queued messages in push order
// and empties the queue, so a second Drain over a quiet queue returns nil.
func TestDrainReturnsThenClears(t *testing.T) {
	b := New(nil, "sess")

	if got := b.Drain(); got != nil {
		t.Fatalf("Drain on empty box = %v, want nil", got)
	}

	b.Push(userMsg("first"), Queue)
	b.Push(userMsg("second"), Queue)

	got := texts(b.Drain())
	want := []string{"first", "second"}
	if len(got) != len(want) {
		t.Fatalf("Drain returned %d msgs, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Drain[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Returned, so now cleared.
	if again := b.Drain(); again != nil {
		t.Fatalf("second Drain = %v, want nil (queue should be cleared)", again)
	}
}

// TestSteerFiresAndQueues: a Steer push both queues the message AND fires the
// steer signal, and the message is queued before the signal is observable
// (queue-then-steer order), so a loop woken by Steer always finds the text.
func TestSteerFiresAndQueues(t *testing.T) {
	b := New(nil, "sess")

	// A plain Queue push must NOT fire the steer signal.
	b.Push(userMsg("queued only"), Queue)
	select {
	case <-b.Steer():
		t.Fatal("Queue push fired the steer signal; only Steer must fire it")
	default:
	}

	b.Push(userMsg("steer now"), Steer)

	select {
	case <-b.Steer():
		// Signal fired; the steering text must already be drainable.
		got := texts(b.Drain())
		want := []string{"queued only", "steer now"}
		if len(got) != len(want) {
			t.Fatalf("Drain after steer = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("Drain[%d] = %q, want %q (order must be preserved)", i, got[i], want[i])
			}
		}
	default:
		t.Fatal("Steer push did not fire the steer signal")
	}
}

// TestSteerCoalesces: a storm of Steer pushes collapses to a single pending
// signal (cap-1 edge-notify). After one receive the channel is empty, even
// though many steers were pushed — and Drain returns the whole batch at once.
func TestSteerCoalesces(t *testing.T) {
	b := New(nil, "sess")

	const n = 100
	for i := 0; i < n; i++ {
		b.Push(userMsg("steer"), Steer)
	}

	// Exactly one signal is receivable.
	select {
	case <-b.Steer():
	default:
		t.Fatal("expected one coalesced steer signal, got none")
	}
	select {
	case <-b.Steer():
		t.Fatal("steer signal did not coalesce: a second signal was receivable")
	default:
	}

	// All pushed messages are still present — coalescing the SIGNAL never drops
	// a MESSAGE.
	if got := b.Drain(); len(got) != n {
		t.Fatalf("Drain after %d steers returned %d msgs, want %d", n, len(got), n)
	}
}

// TestPushNeverBlocksOnPendingSteer: a Steer push when the signal is already
// pending must not block the producer (the whole point of the cap-1 non-blocking
// send). We run it on the test goroutine; a blocking send would deadlock/hang
// the test rather than complete.
func TestPushNeverBlocksOnPendingSteer(t *testing.T) {
	b := New(nil, "sess")
	b.Push(userMsg("a"), Steer) // fills the cap-1 channel
	b.Push(userMsg("b"), Steer) // must not block even though steerC is full
	b.Push(userMsg("c"), Steer)

	// One signal, three messages.
	<-b.Steer()
	if got := b.Drain(); len(got) != 3 {
		t.Fatalf("Drain = %d msgs, want 3", len(got))
	}
}

// TestConcurrentPushDrain hammers Push and Drain from many goroutines. Run with
// -race it proves the mutex fully guards the shared queue: no message is lost or
// duplicated, and no torn read occurs. The exact pushed count is recovered by
// summing every Drain plus a final Drain.
func TestConcurrentPushDrain(t *testing.T) {
	b := New(nil, "sess")

	const (
		producers   = 8
		perProducer = 500
		wantTotal   = producers * perProducer
		drainGoros  = 4
	)

	var (
		wg      sync.WaitGroup
		stop    = make(chan struct{})
		mu      sync.Mutex
		drained int
	)

	// Producers: each pushes perProducer messages, alternating Queue/Steer so
	// the steer channel is exercised concurrently too.
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				mode := Queue
				if i%2 == 0 {
					mode = Steer
				}
				b.Push(userMsg("m"), mode)
			}
		}(p)
	}

	// Concurrent drainers (consumers): they also drain the steer signal so it
	// never wedges, and accumulate the count of messages they pulled.
	var drainWG sync.WaitGroup
	for d := 0; d < drainGoros; d++ {
		drainWG.Add(1)
		go func() {
			defer drainWG.Done()
			for {
				select {
				case <-stop:
					return
				case <-b.Steer():
				default:
				}
				got := b.Drain()
				if len(got) > 0 {
					mu.Lock()
					drained += len(got)
					mu.Unlock()
				}
				select {
				case <-stop:
					// One last sweep after producers finish (drain the tail).
					got := b.Drain()
					if len(got) > 0 {
						mu.Lock()
						drained += len(got)
						mu.Unlock()
					}
					return
				default:
				}
			}
		}()
	}

	wg.Wait()      // all producers done
	close(stop)    // tell drainers to wind down
	drainWG.Wait() // drainers do their final sweep and exit

	// Any messages not yet drained remain in the box — drain them on this
	// goroutine to recover the full total.
	mu.Lock()
	total := drained + len(b.Drain())
	mu.Unlock()

	if total != wantTotal {
		t.Fatalf("recovered %d messages, want %d (lost or duplicated under concurrency)", total, wantTotal)
	}
}

// TestPushLogsMetadataOnly: the user_message audit event records the mode and
// text length only — never the message body (I5 append-only / I7 untrusted body
// stays out of the log).
func TestPushLogsMetadataOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer log.Close()

	const body = "this exact body must never be logged"
	b := New(log, "conv-7")
	b.Push(userMsg(body), Steer)

	if err := log.Err(); err != nil {
		t.Fatalf("log write error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	raw := string(data)

	// The body must not appear anywhere in the audit trail.
	if strings.Contains(raw, body) {
		t.Fatalf("message body leaked into the event log:\n%s", raw)
	}

	// The single line is a user_message event with metadata only.
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one event, got %d", len(lines))
	}
	var ev struct {
		Task   string         `json:"task"`
		Kind   string         `json:"kind"`
		Detail map[string]any `json:"detail"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.Task != "conv-7" {
		t.Fatalf("event Task = %q, want %q", ev.Task, "conv-7")
	}
	if ev.Kind != "user_message" {
		t.Fatalf("event Kind = %q, want user_message", ev.Kind)
	}
	if ev.Detail["mode"] != "steer" {
		t.Fatalf("Detail[mode] = %v, want steer", ev.Detail["mode"])
	}
	// JSON numbers decode to float64.
	if gotLen, ok := ev.Detail["len"].(float64); !ok || int(gotLen) != len(body) {
		t.Fatalf("Detail[len] = %v, want %d", ev.Detail["len"], len(body))
	}
}

// TestModeString documents the mode → label mapping the audit metadata uses.
func TestModeString(t *testing.T) {
	cases := []struct {
		mode Mode
		want string
	}{
		{Queue, "queue"},
		{Steer, "steer"},
		{Mode(99), "queue"}, // anything not Steer reads as the default
	}
	for _, c := range cases {
		if got := c.mode.String(); got != c.want {
			t.Errorf("Mode(%d).String() = %q, want %q", c.mode, got, c.want)
		}
	}
}
