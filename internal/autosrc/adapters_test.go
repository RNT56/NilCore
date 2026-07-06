package autosrc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// errUnexpectedItem flags a Source that returned ok=true when the test expected it to
// unpark on cancellation with no item.
var errUnexpectedItem = errors.New("unexpected item from blocked source")

// TestPriorityBandsOrdered locks the structural ladder: a self-scheduled wake outranks
// an operator-dropped file signal. A refactor that reorders the bands (and so changes
// which funnel drains first under contention) is caught here, not silently in production.
func TestPriorityBandsOrdered(t *testing.T) {
	if PriorityWake <= PriorityFile {
		t.Fatalf("priority bands out of order: file=%d wake=%d", PriorityFile, PriorityWake)
	}
	if PriorityFile != 0 {
		t.Fatalf("PriorityFile must be the zero/default band, got %d", PriorityFile)
	}
}

// TestFileSourceMapsSignals proves FileSource turns each read FileSignal into a
// PriorityFile QueuedSignal with the "file:<name>" Source label and the file's goal,
// then reports DONE when the channel closes.
func TestFileSourceMapsSignals(t *testing.T) {
	ch := make(chan FileSignal, 2)
	ch <- FileSignal{Name: "ci.txt", Goal: "fix the failing build"}
	ch <- FileSignal{Name: "todo", Goal: "rename the package"}
	close(ch)

	src := FileSource{Signals: ch}
	ctx := context.Background()

	want := []struct {
		source, goal string
	}{
		{"file:ci.txt", "fix the failing build"},
		{"file:todo", "rename the package"},
	}
	for i, w := range want {
		qs, ok, err := src.Next(ctx)
		if err != nil || !ok {
			t.Fatalf("Next[%d]: ok=%v err=%v", i, ok, err)
		}
		if qs.Priority != PriorityFile {
			t.Fatalf("Next[%d] priority = %d, want PriorityFile(%d)", i, qs.Priority, PriorityFile)
		}
		if qs.Signal.Source != w.source || qs.Signal.Goal != w.goal {
			t.Fatalf("Next[%d] = {%q,%q}, want {%q,%q}", i, qs.Signal.Source, qs.Signal.Goal, w.source, w.goal)
		}
	}

	// Channel closed ⇒ source DONE: (zero, false, nil), never an error.
	qs, ok, err := src.Next(ctx)
	if ok || err != nil {
		t.Fatalf("exhausted FileSource: got ok=%v err=%v qs=%+v, want false,nil", ok, err, qs)
	}
}

// TestFileSourceFromTempDir is the directory-driven analogue: a temp signals dir is
// read exactly as cmd/nilcore/watch.go does, each entry fed through the adapter, and
// the resulting QueuedSignals match the dropped files. Hermetic (t.TempDir) and
// deterministic (sorted compare).
func TestFileSourceFromTempDir(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"a.signal": "  goal a  ", // leading/trailing space the caller trims
		"b.signal": "goal b",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
	}

	// Caller-side poll: read the dir, trim contents, feed the adapter's channel —
	// mirroring watch.go's pollSignals without importing it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	ch := make(chan FileSignal, len(entries))
	for _, e := range entries {
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %q: %v", e.Name(), err)
		}
		ch <- FileSignal{Name: e.Name(), Goal: strings.TrimSpace(string(body))}
	}
	close(ch)

	src := FileSource{Signals: ch}
	var got []string
	for {
		qs, ok, err := src.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		if qs.Priority != PriorityFile {
			t.Fatalf("priority = %d, want PriorityFile", qs.Priority)
		}
		got = append(got, qs.Signal.Source+"="+qs.Signal.Goal)
	}

	want := []string{"file:a.signal=goal a", "file:b.signal=goal b"}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mismatch at %d: got %v, want %v", i, got, want)
		}
	}
}

// TestWakeSourceMapsFire proves a fired durable wake becomes a PriorityWake
// QueuedSignal whose Source ties to the thread and whose Goal is the self-note.
func TestWakeSourceMapsFire(t *testing.T) {
	ch := make(chan Wake, 1)
	ch <- Wake{ThreadID: "thread-7", Note: "check whether the flaky test still fails"}
	close(ch)

	src := WakeSource{Fires: ch}
	qs, ok, err := src.Next(context.Background())
	if err != nil || !ok {
		t.Fatalf("Next: ok=%v err=%v", ok, err)
	}
	if qs.Priority != PriorityWake {
		t.Fatalf("priority = %d, want PriorityWake(%d)", qs.Priority, PriorityWake)
	}
	if qs.Signal.Source != "wake:thread-7" || qs.Signal.Goal != "check whether the flaky test still fails" {
		t.Fatalf("signal = %+v, want {wake:thread-7, check whether the flaky test still fails}", qs.Signal)
	}

	if _, ok, err := src.Next(context.Background()); ok || err != nil {
		t.Fatalf("exhausted WakeSource: ok=%v err=%v, want false,nil", ok, err)
	}
}

// TestSourcesHonorCancel proves every pull adapter unparks and returns the context
// error when its (empty, never-closed) input blocks and ctx is cancelled — the
// daemon's shutdown contract for a Source.
func TestSourcesHonorCancel(t *testing.T) {
	cases := map[string]Source{
		"file": FileSource{Signals: make(chan FileSignal)},
		"wake": WakeSource{Fires: make(chan Wake)},
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, ok, err := src.Next(ctx)
				if ok {
					done <- errUnexpectedItem
					return
				}
				done <- err
			}()
			time.Sleep(20 * time.Millisecond)
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("%s.Next after cancel: got %v, want context.Canceled", name, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s.Next did not unpark on cancel", name)
			}
		})
	}
}

// TestNilChannelSourceBlocksThenCancels proves the default-off posture: a Source over a
// nil channel produces nothing and exits cleanly on ctx cancel (never a busy-spin, never
// a panic) — wiring a source with no input is harmless.
func TestNilChannelSourceBlocksThenCancels(t *testing.T) {
	src := FileSource{Signals: nil}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := src.Next(ctx)
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("nil-channel Next: got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nil-channel Next did not return on cancel")
	}
}
