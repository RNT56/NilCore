// Package bus is the typed in-process transport between the supervisor and its
// subagents (multi-agent design §3). It is NOT a blackboard or a generalized
// channel: it carries a closed set of control-plane Kinds and treats every
// payload as untrusted data. Two invariants are load-bearing here:
//
//   - I7 (untrusted-as-data): the bus is the single chokepoint. It guard.Wraps
//     every body at delivery and only ever reads typed control fields
//     (Sender/To/Kind/CorrelationID) — it never parses a payload as instructions.
//     Containment rests on structure, not phrase-matching: guard.Suspicious is
//     audit-only (it sets Quarantined and logs), while guard.Wrap is the actual
//     defense, applied unconditionally.
//   - Authority asymmetry: Steer/Cancel may ONLY originate from the Supervisor.
//     A subagent that forges one is rejected by Send and logged. Sender is
//     harness-stamped on Send, never trusted from the model-supplied envelope.
//
// I5 (append-only log): every bus_* event records METADATA ONLY (ids, kinds,
// sizes, drop reasons) — never bodies. The shared *eventlog.Log is nil-safe.
package bus

import (
	"time"

	"nilcore/internal/summarize"
)

// AgentID is a stable principal name: the same string is the backend.Task.ID,
// the branch (task/<ID>), and the bus mailbox address.
type AgentID string

// Supervisor is the one principal allowed to originate Steer/Cancel and to spawn.
const Supervisor AgentID = "super"

// Kind is the closed set of message types. It is validated on Send; an unknown
// Kind is rejected rather than relayed (an open enum would be a smuggling seam).
type Kind string

const (
	KindQuestion      Kind = "question"       // subagent → supervisor (blocking Ask)
	KindAnswer        Kind = "answer"         // reply, carries CorrelationID
	KindFinding       Kind = "finding"        // async share, fenced data (peer-to-peer ok)
	KindReviewRequest Kind = "review_request" // blocking Ask for a cross-model review
	KindReviewResult  Kind = "review_result"  // reply to a review_request
	KindSteer         Kind = "steer"          // SUPERVISOR-ONLY directive
	KindCancel        Kind = "cancel"         // SUPERVISOR-ONLY stop
	KindHeartbeat     Kind = "heartbeat"      // liveness ping
)

// valid reports whether k is a member of the closed Kind set.
func (k Kind) valid() bool {
	switch k {
	case KindQuestion, KindAnswer, KindFinding, KindReviewRequest,
		KindReviewResult, KindSteer, KindCancel, KindHeartbeat:
		return true
	default:
		return false
	}
}

// supervisorOnly reports whether k may only originate from the Supervisor. These
// are the command-plane Kinds; a compromised subagent must never command a peer.
func (k Kind) supervisorOnly() bool {
	return k == KindSteer || k == KindCancel
}

// Message is the bus envelope. Control fields (Sender/To/Kind/CorrelationID) are
// trusted, typed routing metadata; Payload and Artifacts are UNTRUSTED and are
// guard.Wrapped on delivery. Sender is harness-stamped by Send — a value supplied
// by the model is overwritten, so a subagent cannot impersonate the supervisor.
type Message struct {
	ID            string                   // harness-stamped unique id
	Sender        string                   // harness-stamped principal (NOT model-claimed)
	To            []AgentID                // explicit recipients (ignored when Broadcast)
	Broadcast     bool                     // deliver to every registered agent except the sender
	Kind          Kind                     // closed set; validated on Send
	CorrelationID string                   // ties an Answer/ReviewResult to its Ask
	Summary       summarize.ContextSummary // bounded carry-over, never transcripts
	Payload       string                   // UNTRUSTED — guard.Wrapped on delivery
	Artifacts     map[string]string        // UNTRUSTED, size-capped on delivery
	Path          []AgentID                // hops so far; used for cycle detection
	TTL           int                      // remaining hops; decremented per relay, drop at <=0
	Quarantined   bool                     // set by the bus when guard.Suspicious fires (audit only)
	Time          time.Time                // harness-stamped send time
}

// recipients returns the concrete delivery set for m given the registered agents,
// excluding the sender (no self-delivery). For a broadcast it is every other
// registered agent; otherwise it is the To list with the sender removed.
func (m Message) recipients(registered map[AgentID]struct{}) []AgentID {
	sender := AgentID(m.Sender)
	if m.Broadcast {
		out := make([]AgentID, 0, len(registered))
		for id := range registered {
			if id != sender {
				out = append(out, id)
			}
		}
		return out
	}
	out := make([]AgentID, 0, len(m.To))
	for _, id := range m.To {
		if id != sender {
			out = append(out, id)
		}
	}
	return out
}
