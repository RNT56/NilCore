package autosrc

import (
	"container/heap"

	"nilcore/internal/trigger"
)

// QueuedSignal is a trigger.Signal admitted into the daemon's single queue,
// carried with a routing Priority and a stable enqueue sequence. The embedded
// Signal IS the work (Source + Goal); Priority orders how soon it is drained; seq
// is assigned by the queue at Push time and breaks ties FIFO, so two signals of
// equal Priority drain in the order they arrived. seq is unexported on purpose:
// it is the queue's bookkeeping, not a field a source may forge.
//
// Note (I7): the Signal here is UNTRUSTED data exactly as everywhere else in the
// self-start funnel — the daemon only ROUTES Goal to the injected handler, it
// never interprets the text as an instruction. Priority is a structural integer a
// source assigns; the queue templates only it and seq.
type QueuedSignal struct {
	// Signal is the underlying trigger.Signal: {Source, Goal}. Mirrored by value so
	// the queue never aliases a source's mutable state.
	Signal trigger.Signal
	// Priority orders draining: HIGHER drains first. Equal priorities fall back to
	// FIFO via the enqueue sequence. The zero value is a valid (lowest-band) signal.
	Priority int

	seq uint64 // enqueue order, assigned by pq.push; the stable FIFO tie-break
}

// pq is a bounded max-priority queue ordered by (Priority desc, seq asc). It is the
// internal heap; callers go through boundedQueue which adds the cap, the mutex, and
// the close/drain semantics. container/heap (stdlib, I6) supplies the algorithm.
type pq []QueuedSignal

func (q pq) Len() int { return len(q) }

// Less orders a MAX-heap on Priority (higher Priority is "less" so it surfaces at
// the root) and, for equal Priority, a MIN-heap on seq (an earlier enqueue surfaces
// first — stable FIFO within a priority band). Total and deterministic: no two live
// elements share a seq, so ordering is never ambiguous.
func (q pq) Less(i, j int) bool {
	if q[i].Priority != q[j].Priority {
		return q[i].Priority > q[j].Priority
	}
	return q[i].seq < q[j].seq
}

func (q pq) Swap(i, j int) { q[i], q[j] = q[j], q[i] }

// Push/Pop satisfy heap.Interface. They operate on the slice tail; callers use
// heap.Push/heap.Pop (never these directly) so the heap invariant is maintained.
func (q *pq) Push(x any) { *q = append(*q, x.(QueuedSignal)) }

func (q *pq) Pop() any {
	old := *q
	n := len(old)
	it := old[n-1]
	old[n-1] = QueuedSignal{} // release for GC; do not retain the popped value
	*q = old[:n-1]
	return it
}

// ensure pq satisfies heap.Interface at compile time.
var _ heap.Interface = (*pq)(nil)
