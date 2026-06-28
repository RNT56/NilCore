package bus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"nilcore/internal/eventlog"
	"nilcore/internal/guard"
)

// Tunables. A buffered mailbox plus a bounded send-wait gives back-pressure
// without deadlock: a slow or dead recipient drops a message, it never blocks
// the sender or a broadcast (design §3 "back-pressure & termination").
const (
	defaultSendWait   = 50 * time.Millisecond // how long Send waits for a full mailbox before dropping
	defaultArtifacts  = 32 * 1024             // per-message Artifacts byte cap (untrusted, size-capped)
	correlationPrefix = "ask"                 // ids minted for Ask correlation
)

// Errors a caller can branch on. Drops and unauthorized sends are not errors to
// the *bus* (it stays live); they are returned so a caller knows the outcome and
// are always logged as bus_* metadata.
var (
	// ErrUnauthorized is returned when a non-Supervisor sender originates a
	// Steer/Cancel (authority asymmetry, the I7 trust anchor).
	ErrUnauthorized = errors.New("bus: sender not authorized for this kind")
	// ErrInvalidKind is returned for a Kind outside the closed set.
	ErrInvalidKind = errors.New("bus: invalid message kind")
	// ErrUnknownAgent is returned by Register/Deregister/Ask on an id problem.
	ErrUnknownAgent = errors.New("bus: unknown agent")
)

// Bus is a typed in-process message transport with per-agent mailboxes. The zero
// value is not usable; construct with New. It is safe for concurrent use.
type Bus struct {
	log          *eventlog.Log // shared, nil-safe; bus_* events are METADATA ONLY
	mailboxDepth int           // buffered capacity of each mailbox channel
	maxMessages  int64         // tree-wide Send cap (0 = unlimited); a termination rail

	mu    sync.RWMutex
	boxes map[AgentID]chan Message

	// waiters maps an Ask's CorrelationID to the channel its reply is delivered
	// on, so Send can hand a correlated Answer straight to the blocked Ask
	// instead of queueing it in a mailbox the asker may not be draining.
	waiters map[string]chan Message

	seq      atomic.Uint64 // mints unique message/correlation ids
	msgCount atomic.Int64  // total Sends accepted (against maxMessages)
}

// New constructs a Bus. log may be nil (the bus stays fully functional, just
// unlogged). mailboxDepth is each mailbox's buffer; maxMessages caps total
// accepted Sends across the whole tree (<=0 means unlimited).
func New(log *eventlog.Log, mailboxDepth, maxMessages int) *Bus {
	if mailboxDepth < 1 {
		mailboxDepth = 1
	}
	return &Bus{
		log:          log,
		mailboxDepth: mailboxDepth,
		maxMessages:  int64(maxMessages),
		boxes:        make(map[AgentID]chan Message),
		waiters:      make(map[string]chan Message),
	}
}

// Register creates a mailbox for id and returns its receive-only channel. The
// channel is closed on Deregister. Re-registering an existing id is an error so a
// caller cannot orphan a live mailbox.
func (b *Bus) Register(id AgentID) (<-chan Message, error) {
	if id == "" {
		return nil, fmt.Errorf("register: %w", ErrUnknownAgent)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.boxes[id]; ok {
		return nil, fmt.Errorf("register %q: already registered", id)
	}
	ch := make(chan Message, b.mailboxDepth)
	b.boxes[id] = ch
	return ch, nil
}

// Deregister removes id's mailbox and closes its channel so a ranging receiver
// exits cleanly. It is idempotent. Synchronous delivery in Send means no goroutine
// is left writing to the closed channel.
func (b *Bus) Deregister(id AgentID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.boxes[id]; ok {
		delete(b.boxes, id)
		close(ch)
	}
}

// Send delivers m to its recipients. It is the single enforcement chokepoint:
//
//  1. Sender/ID/Time are harness-stamped (a model-supplied Sender is overwritten).
//  2. Kind must be in the closed set; Steer/Cancel must come from the Supervisor.
//  3. MaxMessages, TTL exhaustion, and Path cycles cap the traffic.
//  4. Every body is guard.Wrapped and Artifacts size-capped before delivery;
//     guard.Suspicious sets Quarantined (audit only).
//
// A full or dead mailbox causes a per-recipient drop (logged), never a block.
// Send returns an error only for a rejected send (unauthorized/invalid/capped);
// a per-recipient drop is reported via the log, not as an error, so a broadcast
// to a slow agent still succeeds for the others.
func (b *Bus) Send(ctx context.Context, m Message) error {
	if !m.Kind.valid() {
		b.event("bus_drop", m, map[string]any{"reason": "invalid_kind", "kind": string(m.Kind)})
		return fmt.Errorf("send: %w (%q)", ErrInvalidKind, m.Kind)
	}
	// Authority asymmetry: Steer/Cancel are command-plane and supervisor-only.
	// This is checked on the *claimed* sender before we trust-stamp, because the
	// caller passes its own identity in Sender; the harness binds principals to
	// callers elsewhere. A forged supervisor Sender cannot help a subagent because
	// the subagent has no steer/cancel tool registered (P1-T03).
	if m.Kind.supervisorOnly() && AgentID(m.Sender) != Supervisor {
		b.event("bus_unauthorized", m, map[string]any{"reason": "non_supervisor_command"})
		return fmt.Errorf("send %s from %q: %w", m.Kind, m.Sender, ErrUnauthorized)
	}

	// Tree-wide message ceiling: a termination rail independent of the model.
	if b.maxMessages > 0 && b.msgCount.Add(1) > b.maxMessages {
		b.event("bus_drop", m, map[string]any{"reason": "max_messages"})
		return nil
	}

	// Stamp the trusted id/time the harness owns, but evaluate the caps on the
	// caller-supplied TTL/Path BEFORE consuming a hop, so the checks are about
	// the message as received, not as relayed.
	b.identify(&m)

	// TTL is remaining hops. A message that arrives with no hops left is dropped,
	// so a relay storm terminates by construction. Hops are then consumed for the
	// onward path appended below.
	if m.TTL <= 0 {
		b.event("bus_drop", m, map[string]any{"reason": "ttl_exhausted"})
		return nil
	}
	// Path cycle: if the sender already appears on the path, this is a loop.
	if onPath(m.Path, AgentID(m.Sender)) {
		b.event("bus_drop", m, map[string]any{"reason": "path_cycle"})
		return nil
	}
	b.relay(&m) // consume one hop and record the sender on the path

	// Fence + flag the untrusted body once, before fan-out. Wrap is the real
	// defense (unconditional); Suspicious only sets the audit flag.
	b.sanitize(&m)
	b.event("bus_send", m, map[string]any{"recipients": len(m.To), "broadcast": m.Broadcast})

	// A correlated reply (Answer/ReviewResult) is handed directly to the waiting
	// Ask if one is parked on this CorrelationID — the asker is blocked, not
	// draining its mailbox, so a mailbox enqueue could deadlock the one-shot.
	if m.CorrelationID != "" && (m.Kind == KindAnswer || m.Kind == KindReviewResult) {
		if b.deliverReply(m) {
			b.event("bus_answer", m, map[string]any{"correlation_id": m.CorrelationID})
			return nil
		}
	}

	recipients := m.recipients(b.snapshotRegistered())
	if len(recipients) == 0 {
		// Only addressee was the sender (self-addressed) or an empty To list: a
		// no-op delivery is recorded as a drop so a self-loop is auditable, not
		// silently swallowed.
		b.event("bus_drop", m, map[string]any{"reason": "self_or_empty"})
		return nil
	}
	for _, id := range recipients {
		b.deliverOne(ctx, id, m)
	}
	return nil
}

// Ask sends m and blocks until a reply correlated to it arrives, ctx is done, or
// the context deadline elapses. It is the consult-and-resume primitive the native
// loop uses for ask_supervisor/request_review: the caller's step blocks while the
// supervisor answers. A CorrelationID is minted if the caller did not set one.
// On ctx cancellation/timeout it returns the ctx error so the caller can fall
// back to "no answer; proceed with best judgment" rather than hang.
func (b *Bus) Ask(ctx context.Context, m Message) (Message, error) {
	if m.CorrelationID == "" {
		m.CorrelationID = b.mintID(correlationPrefix)
	}
	reply := make(chan Message, 1)

	b.mu.Lock()
	b.waiters[m.CorrelationID] = reply
	b.mu.Unlock()
	// Always retire the waiter: on timeout it must not linger and steal a later
	// reply, and a late reply must find no waiter and fall through to a mailbox.
	defer func() {
		b.mu.Lock()
		delete(b.waiters, m.CorrelationID)
		b.mu.Unlock()
	}()

	b.event("bus_ask", m, map[string]any{"correlation_id": m.CorrelationID})
	if err := b.Send(ctx, m); err != nil {
		return Message{}, err
	}

	select {
	case r := <-reply:
		return r, nil
	case <-ctx.Done():
		b.event("bus_undeliverable", m, map[string]any{
			"correlation_id": m.CorrelationID, "reason": "ask_timeout"})
		return Message{}, fmt.Errorf("ask %s: %w", m.CorrelationID, ctx.Err())
	}
}

// deliverReply hands a correlated reply to a parked Ask, returning false if no
// waiter is registered (the reply then falls through to normal mailbox delivery).
func (b *Bus) deliverReply(m Message) bool {
	b.mu.Lock()
	w, ok := b.waiters[m.CorrelationID]
	if ok {
		delete(b.waiters, m.CorrelationID)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	w <- m // buffered (cap 1); never blocks
	return true
}

// deliverOne attempts a single recipient delivery. It never blocks the bus: it
// waits at most defaultSendWait for a full mailbox, then drops + logs. A missing
// mailbox (deregistered) is an undeliverable drop, also logged.
func (b *Bus) deliverOne(ctx context.Context, id AgentID, m Message) {
	// Hold the READ lock across the send. Deregister closes the mailbox under the
	// WRITE lock, so close() cannot run while we hold RLock — this closes the
	// TOCTOU window where the lookup-then-unlock-then-send pattern could send on an
	// already-closed channel and panic. The lock is read-shared, so concurrent
	// deliveries to OTHER mailboxes still proceed; only a concurrent Register/
	// Deregister waits, and at most defaultSendWait (bounded teardown latency).
	b.mu.RLock()
	ch, ok := b.boxes[id]
	if !ok {
		b.mu.RUnlock()
		b.event("bus_undeliverable", m, map[string]any{"to": string(id), "reason": "no_mailbox"})
		return
	}

	timer := time.NewTimer(defaultSendWait)
	defer timer.Stop()
	// Capture the outcome under the lock, then emit the audit event AFTER releasing it
	// so the (disk-backed) log append does not extend the lock window past the send.
	var kind string
	detail := map[string]any{"to": string(id)}
	select {
	case ch <- m:
		kind = "bus_deliver"
	case <-timer.C:
		kind, detail["reason"] = "bus_drop", "mailbox_full"
	case <-ctx.Done():
		kind, detail["reason"] = "bus_drop", "ctx_done"
	}
	b.mu.RUnlock()
	b.event(kind, m, detail)
}

// identify binds the trusted control fields the harness owns: a fresh id (if
// unset) and the send time. Sender is intentionally NOT overwritten — the caller
// declares its principal and the authority check above already gated command
// Kinds on it; the harness binds principals to callers at a higher layer.
func (b *Bus) identify(m *Message) {
	if m.ID == "" {
		m.ID = b.mintID("msg")
	}
	m.Time = time.Now().UTC()
}

// relay consumes one hop and appends the sender to the path so a future relay can
// detect a cycle. Called only after the TTL/cycle caps have passed.
func (b *Bus) relay(m *Message) {
	m.TTL--
	m.Path = append(append([]AgentID(nil), m.Path...), AgentID(m.Sender))
}

// sanitize fences the untrusted body (always) and caps Artifacts (untrusted,
// size-capped), then flags Quarantined for the audit trail. Wrap is the load-
// bearing containment; Suspicious is advisory only.
func (b *Bus) sanitize(m *Message) {
	if guard.Suspicious(m.Payload) {
		m.Quarantined = true
		b.event("bus_injection_flagged", *m, map[string]any{"field": "payload"})
	}
	m.Payload = guard.Wrap("bus message from "+m.Sender, m.Payload)

	if len(m.Artifacts) > 0 {
		capped := make(map[string]string, len(m.Artifacts))
		for k, v := range m.Artifacts {
			if guard.Suspicious(v) {
				m.Quarantined = true
				b.event("bus_injection_flagged", *m, map[string]any{"field": "artifact", "name": k})
			}
			if len(v) > defaultArtifacts {
				v = v[:defaultArtifacts]
			}
			capped[k] = guard.Wrap("bus artifact "+k+" from "+m.Sender, v)
		}
		m.Artifacts = capped
	}
}

// snapshotRegistered returns a copy of the registered id set so recipient
// computation does not hold the lock across delivery.
func (b *Bus) snapshotRegistered() map[AgentID]struct{} {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[AgentID]struct{}, len(b.boxes))
	for id := range b.boxes {
		out[id] = struct{}{}
	}
	return out
}

// mintID returns a process-unique id with a readable prefix.
func (b *Bus) mintID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, b.seq.Add(1))
}

// onPath reports whether id already appears on the relay path (a cycle).
func onPath(path []AgentID, id AgentID) bool {
	for _, p := range path {
		if p == id {
			return true
		}
	}
	return false
}

// event records one bus_* audit entry. CRITICAL: METADATA ONLY — ids, kinds,
// sizes, drop reasons — never the Payload/Artifacts bodies (I5/I7). The shared
// log is nil-safe (Append tolerates a nil receiver).
func (b *Bus) event(kind string, m Message, extra map[string]any) {
	if b.log == nil {
		return
	}
	detail := map[string]any{
		"id":          m.ID,
		"sender":      m.Sender,
		"kind":        string(m.Kind),
		"ttl":         m.TTL,
		"quarantined": m.Quarantined,
	}
	for k, v := range extra {
		detail[k] = v
	}
	b.log.Append(eventlog.Event{Task: m.Sender, Kind: kind, Detail: detail})
}
