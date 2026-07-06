package autosrc

// adapters.go (AUTO-T04) — presents the EXISTING self-start funnels as autosrc pull
// Sources, so the unified daemon can drain them all from ONE bounded priority queue.
// Nothing here changes WHAT gets started — each adapter only re-shapes an
// already-produced trigger.Signal into a QueuedSignal with a structural Priority and
// hands it to the queue the daemon owns. The adapters are
// THIN and PURE: they never start work, never gate, never verify, never touch
// secrets, and never import the orchestrator (I2/I3, the deps_test guard). The
// Signal's Goal stays untrusted data exactly as upstream produces it (I7); only the
// integer Priority is templated here.
//
// # Priority bands
//
// A small, fixed, structural ladder orders the funnels so a self-scheduled wake
// outranks routine operator-dropped work when the daemon is saturated. Equal
// priorities fall back to FIFO inside the queue, so two signals in the same band
// drain in arrival order. None of this is learned or model-influenced — it is a
// property of WHICH funnel emitted the signal.

import (
	"context"

	"nilcore/internal/trigger"
)

const (
	// PriorityFile is the band for operator-dropped file signals (`nilcore watch`):
	// routine, human-initiated, lowest urgency. The zero/default band.
	PriorityFile = 0
	// PriorityWake is the band for durable self-scheduled timers (internal/wake): the
	// agent asked to be re-engaged at a chosen instant, so a fired wake should not lag
	// behind routine file work.
	PriorityWake = 20
)

// FileSignal is a directory entry observed by the file-signal funnel (`nilcore
// watch`): the file name is the Signal.Source label and its trimmed contents are the
// Goal. It is a pure value the FileSource adapter maps into a QueuedSignal; the
// adapter does NOT read the filesystem itself (the caller polls the directory exactly
// as cmd/nilcore/watch.go does today), keeping the adapter hermetic and free of any
// host-I/O dependency.
type FileSignal struct {
	// Name is the signal file's base name; it becomes the Signal.Source as "file:<name>",
	// matching today's watch verb.
	Name string
	// Goal is the file's trimmed contents — the natural-language task. Untrusted (I7).
	Goal string
}

// FileSource is a pull Source over a stream of already-read FileSignals. The caller
// (the watch loop / wiring layer) owns the directory poll and the once-only file
// removal; this adapter only converts each FileSignal it is handed into a
// PriorityFile QueuedSignal. Next blocks on the channel until a signal arrives, the
// channel closes (the source is then DONE), or ctx is cancelled.
//
// Keeping the poll OUT of the adapter is deliberate: it stays a thin, pure, fully
// hermetic mapper (no os.ReadDir, no clock), so the daemon owns lifetime/concurrency
// and the adapter owns only the trigger.Signal → QueuedSignal shape.
type FileSource struct {
	// Signals is the inbound stream of read file signals. Closing it marks the source
	// exhausted (Next then returns ok=false). A nil channel makes Next block until ctx
	// is cancelled — a default-off source that produces nothing.
	Signals <-chan FileSignal
}

// Next yields the next file signal as a PriorityFile QueuedSignal, or reports the
// source done when the channel closes, honoring ctx.
func (s FileSource) Next(ctx context.Context) (QueuedSignal, bool, error) {
	select {
	case fs, open := <-s.Signals:
		if !open {
			return QueuedSignal{}, false, nil // channel closed ⇒ exhausted
		}
		return QueuedSignal{
			Signal:   trigger.Signal{Source: "file:" + fs.Name, Goal: fs.Goal},
			Priority: PriorityFile,
		}, true, nil
	case <-ctx.Done():
		return QueuedSignal{}, false, ctx.Err()
	}
}

// WakeSource is a pull Source over a stream of fired durable wakes (internal/wake).
// The serve waker drives Pending → fire (it owns the store and the clock); this
// adapter maps each fired Wake into a PriorityWake QueuedSignal whose Goal is the
// wake's bounded Note (the agent's message to its future self) and whose Source ties
// back to the woken thread. Pure: no store, no clock. Next blocks until a fire
// arrives, the channel closes (DONE), or ctx is cancelled.
type WakeSource struct {
	// Fires is the inbound stream of due wakes. Closing it exhausts the source. A nil
	// channel produces nothing until ctx is cancelled.
	Fires <-chan Wake
}

// Wake mirrors the firing-relevant fields of a wake.Wake without importing the wake
// package — the adapter only needs the thread label and the self-note to form a
// Signal, and re-declaring this tiny value keeps autosrc a leaf with no new import.
// The caller translates a wake.Wake into this shape at the seam.
type Wake struct {
	// ThreadID is the woken conversation; it becomes the Signal.Source as "wake:<id>".
	ThreadID string
	// Note is the agent's bounded self-note — the natural-language goal to resume on.
	// Untrusted data (I7), passed through unaltered.
	Note string
}

// Next yields the next fired wake as a PriorityWake QueuedSignal.
func (s WakeSource) Next(ctx context.Context) (QueuedSignal, bool, error) {
	select {
	case wk, open := <-s.Fires:
		if !open {
			return QueuedSignal{}, false, nil
		}
		return QueuedSignal{
			Signal:   trigger.Signal{Source: "wake:" + wk.ThreadID, Goal: wk.Note},
			Priority: PriorityWake,
		}, true, nil
	case <-ctx.Done():
		return QueuedSignal{}, false, ctx.Err()
	}
}
