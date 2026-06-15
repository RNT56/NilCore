// Package inbox is the user→agent message seam for the conversational front
// door (C1-T01). A running loop (native or supervisor) drains it at each
// boundary to fold in user turns the principal typed mid-work, and selects on
// its steer signal to learn when a steer demands the in-flight model call be
// cancelled now rather than at the next boundary.
//
// It mirrors super/reader.go's discipline: a single mutex guards the only shared
// mutable state (the queued slice), and the steer signal is a cap-1 buffered
// channel used as an edge-notify — a "steer storm" of rapid pushes collapses to
// at most one pending wake-up, so a busy loop never accumulates a backlog of
// redundant cancellations. It is a stdlib-only leaf (sync) plus internal/model
// (the message shape) and internal/eventlog (metadata-only audit). It imports no
// loop/channel/session machinery, so neither backend nor super gains an import
// into this package's consumers — each consumer declares a small local interface
// that *Box satisfies, exactly as backend declares Peer.
package inbox

import (
	"sync"

	"nilcore/internal/eventlog"
	"nilcore/internal/model"
)

// Mode classifies a pushed message. Queue (the default) folds the message in as
// an ordinary user turn at the next loop boundary; Steer additionally fires the
// steer signal so the loop cancels its in-flight model call and unwinds to the
// boundary immediately.
type Mode int

const (
	// Queue defers the message to the next loop boundary.
	Queue Mode = iota
	// Steer fires the steer signal in addition to queuing.
	Steer
)

func (m Mode) String() string {
	if m == Steer {
		return "steer"
	}
	return "queue"
}

// Box is the user→agent seam. The producer (a Turn / intake goroutine) calls
// Push; the single loop goroutine calls Drain and selects on Steer. All shared
// state is mutex-guarded and the steer channel is a cap-1 edge-notify, so Box is
// race-free and leaks no goroutine (it owns none).
type Box struct {
	mu     sync.Mutex
	queued []model.Message

	// steerC is a cap-1 buffered channel used as an edge-notify: a Steer push
	// does a non-blocking send, so when a signal is already pending the extra
	// steer coalesces into it rather than blocking the producer or queuing a
	// second redundant wake-up.
	steerC chan struct{}

	log   *eventlog.Log // optional; Append is nil-safe
	label string        // event Task label (e.g. the conversation/session ID)
}

// New returns a ready Box. log and label feed the metadata-only user_message
// audit event on Push; a nil log is tolerated (eventlog.Append is nil-safe).
func New(log *eventlog.Log, label string) *Box {
	return &Box{
		steerC: make(chan struct{}, 1),
		log:    log,
		label:  label,
	}
}

// Push enqueues m and, when mode is Steer, fires the steer signal. Order is
// preserved: the message is appended to the queue (so a later Drain sees it)
// before the steer wake-up is delivered, so a loop woken by the steer always
// finds the steering text already queued. Push never blocks: the steer send is
// non-blocking, so a steer storm against a busy loop collapses to one pending
// signal instead of stalling the producer.
//
// It logs a user_message event carrying metadata only — the mode and the text
// length, never the body — preserving the append-only audit (I5) and the rule
// that the message body is the principal's data, not log content (I7).
func (b *Box) Push(m model.Message, mode Mode) {
	b.mu.Lock()
	b.queued = append(b.queued, m)
	b.mu.Unlock()

	b.log.Append(eventlog.Event{
		Task: b.label,
		Kind: "user_message",
		Detail: map[string]any{
			"mode": mode.String(),
			"len":  textLen(m),
		},
	})

	if mode == Steer {
		// Non-blocking edge-notify: if a signal is already pending the steer
		// coalesces into it. The message is already queued above, so the order
		// "queue then steer" is preserved for the consumer.
		select {
		case b.steerC <- struct{}{}:
		default:
		}
	}
}

// Drain returns the queued messages and clears the queue atomically under the
// mutex, so a concurrent Push either lands wholly before or wholly after this
// hand-off — never a torn read. It returns nil when the queue is empty (the
// loop's hot path: a boundary with no pending user input allocates nothing).
func (b *Box) Drain() []model.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.queued) == 0 {
		return nil
	}
	out := b.queued
	b.queued = nil
	return out
}

// Steer returns the steer signal the loop selects on. A receive on it means at
// least one steer push arrived since it was last drained; because it is a cap-1
// edge-notify, a burst of steers yields a single receivable signal — the loop
// reacts once and then Drains the whole coalesced batch.
func (b *Box) Steer() <-chan struct{} {
	return b.steerC
}

// textLen sums the lengths of the message's text blocks. It is metadata for the
// audit event (how much the user typed), computed without ever copying or
// logging the body itself.
func textLen(m model.Message) int {
	n := 0
	for _, blk := range m.Content {
		n += len(blk.Text)
	}
	return n
}
