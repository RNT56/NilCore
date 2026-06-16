package bus

// AgentPeer is a single subagent's handle on the bus (multi-agent design §3). It
// is the seam that satisfies backend.Peer (internal/backend/native.go): the native
// loop holds a Peer interface, not the concrete type, so the bus never leaks into
// the frozen-contract backend package's import graph. The loop registers exactly
// the tools AgentPeer.Tools returns and calls AgentPeer.Dispatch for each.
//
// Authority asymmetry is structural here (I7 trust anchor): a subagent peer is
// handed ONLY the three subagent tools below. There is deliberately NO steer,
// cancel, spawn, or delegate tool — those are the supervisor's monopoly. A
// compromised subagent therefore physically cannot originate a command-plane Kind
// from its tool surface; the bus's Send check is the second line, not the first.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"nilcore/internal/guard"
	"nilcore/internal/model"
)

// askTimeout bounds a blocking bus Ask so a non-draining supervisor cannot hang a
// subagent's step. On timeout Dispatch returns a graceful "no answer; proceed"
// note rather than an error, matching native.go's advisor ErrCeiling fallback —
// the loop keeps moving on the subagent's own best judgment.
const askTimeout = 2 * time.Minute

// defaultTTL is the hop budget the peer stamps on every message it originates. A
// subagent→supervisor message is one hop; a few extra cover the supervisor's relay
// of a finding to peers. The bus drops a message that runs out of hops, so this is
// a termination rail (a relay storm cannot outlive its TTL).
const defaultTTL = 8

// Tool names are the closed set of bus tools a subagent may call. They are the
// ONLY inter-agent surface a subagent has — no steer/cancel/spawn (asymmetry).
const (
	toolAskSupervisor = "ask_supervisor"
	toolShareFinding  = "share_finding"
	toolRequestReview = "request_review"
)

// AgentPeer is the subagent's view of the bus: its own principal id (Self), the
// shared Bus, and its receive-only mailbox (In) for messages the supervisor pushes
// (steer/cancel/findings). The zero value is unusable; build with NewPeer.
type AgentPeer struct {
	Self AgentID        // this subagent's principal/branch/mailbox address
	Bus  *Bus           // shared transport
	In   <-chan Message // this subagent's mailbox (from Bus.Register)
}

// NewPeer registers self on the bus and returns its handle. The mailbox channel is
// captured into In so the caller (the role worker) can drain steer/cancel without
// re-deriving it. Deregister on retirement to close the mailbox and free the slot.
func NewPeer(b *Bus, self AgentID) (*AgentPeer, error) {
	in, err := b.Register(self)
	if err != nil {
		return nil, fmt.Errorf("peer register %q: %w", self, err)
	}
	return &AgentPeer{Self: self, Bus: b, In: in}, nil
}

// Tools returns EXACTLY the three subagent bus tools, in a stable order. This is
// the whole inter-agent surface a subagent gets: ask the supervisor (blocking),
// share a finding (async, peer-visible), request a review (blocking). There is no
// steer/cancel/spawn/delegate tool here by construction — that asymmetry, not a
// prompt rule, is what keeps a compromised subagent from commanding the cohort.
func (p *AgentPeer) Tools() []model.Tool {
	return []model.Tool{
		{
			Name: toolAskSupervisor,
			Description: "Ask the supervisor a focused question and block briefly until it answers. " +
				"Use it PROACTIVELY whenever you are uncertain about a design decision or interface, " +
				"your change might conflict with or duplicate a sibling's work, you are about to assume " +
				"something that would waste effort if wrong, or its input would change your approach. " +
				"The supervisor has the full plan and sees the integrated tree; asking early is cheap and " +
				"expected (not a failure). Your current work-in-progress is attached automatically — just " +
				"state your specific question.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"}},"required":["question"]}`),
		},
		{
			Name: toolShareFinding,
			Description: "Share a finding with the supervisor (and any peers) without blocking. " +
				"Use for durable facts, results, or warnings others should know.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"finding":{"type":"string"}},"required":["finding"]}`),
		},
		{
			Name: toolRequestReview,
			Description: "Request a cross-model review of your diff or approach and block until it returns. " +
				"Provide the artifact or question to review.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"request":{"type":"string"}},"required":["request"]}`),
		},
	}
}

// Dispatch routes one bus tool call to the bus and returns the reply text for the
// loop to turn into a tool_result. The blocking-vs-async distinction lives here,
// not in the loop:
//
//   - ask_supervisor / request_review → Bus.Ask (blocking, correlated one-shot).
//   - share_finding                   → Bus.Send (async; "shared" ack, no reply).
//
// The returned reply is the UNTRUSTED bus payload. Dispatch guard.Wraps it at this
// seam — the peer is the component that reads the untrusted Message.Payload off the
// bus, so it owns the I7 boundary here. (native.go wraps the return value again
// with its own label; Wrap escapes nested fences, so layering is safe and the
// content can never break out to become an instruction.)
func (p *AgentPeer) Dispatch(ctx context.Context, name string, input json.RawMessage) (string, error) {
	switch name {
	case toolAskSupervisor:
		return p.ask(ctx, name, KindQuestion, input, "question")
	case toolRequestReview:
		return p.ask(ctx, name, KindReviewRequest, input, "request")
	case toolShareFinding:
		return p.share(ctx, input)
	default:
		// A subagent must never reach a steer/cancel/spawn path — there is no such
		// tool registered, so an unknown name is a hard error, not a silent route.
		return "", fmt.Errorf("peer dispatch: unknown tool %q", name)
	}
}

// ask sends a blocking question/review-request to the supervisor and returns its
// fenced answer. A ctx-bounded timeout yields a graceful "proceed" note instead of
// hanging the subagent's step (mirrors native.go's advisor-ceiling fallback).
func (p *AgentPeer) ask(ctx context.Context, tool string, kind Kind, input json.RawMessage, field string) (string, error) {
	body, err := decodeField(input, field)
	if err != nil {
		return "", fmt.Errorf("%s: %w", tool, err)
	}

	askCtx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()

	reply, err := p.Bus.Ask(askCtx, Message{
		Sender:  string(p.Self),
		To:      []AgentID{Supervisor},
		Kind:    kind,
		Payload: body,
		TTL:     defaultTTL,
	})
	if err != nil {
		// No answer in time (or ctx canceled): do not fail the step. Hand back a
		// fenced note so the loop proceeds on the subagent's best judgment.
		return guard.Wrap(tool+" (no answer)",
			"The supervisor did not answer in time. Proceed with your best judgment, "+
				"or call finish and let the verifier and integration decide."), nil
	}
	// The bus already wrapped reply.Payload on delivery; we re-fence with the tool
	// label so the reply is unambiguously data at this seam too (defense in depth).
	return guard.Wrap(tool+" reply", reply.Payload), nil
}

// share posts an async finding to the supervisor and returns a fixed acknowledgment
// (no reply is awaited). A finding is fenced data; peers may receive it too, but it
// can never command anyone (Finding is not a command-plane Kind).
func (p *AgentPeer) share(ctx context.Context, input json.RawMessage) (string, error) {
	body, err := decodeField(input, "finding")
	if err != nil {
		return "", fmt.Errorf("%s: %w", toolShareFinding, err)
	}
	if err := p.Bus.Send(ctx, Message{
		Sender:  string(p.Self),
		To:      []AgentID{Supervisor},
		Kind:    KindFinding,
		Payload: body,
		TTL:     defaultTTL,
	}); err != nil {
		return "", fmt.Errorf("%s: %w", toolShareFinding, err)
	}
	// No untrusted body flows back, so this fixed ack needs no fence.
	return "finding shared with the supervisor.", nil
}

// decodeField pulls a single named string field out of a tool input object,
// tolerating extra fields. An empty or absent value is an error so a tool call
// never sends an empty question/finding onto the bus.
func decodeField(input json.RawMessage, field string) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(input, &obj); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	raw, ok := obj[field]
	if !ok {
		return "", fmt.Errorf("missing %q", field)
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("field %q not a string: %w", field, err)
	}
	if v == "" {
		return "", fmt.Errorf("empty %q", field)
	}
	return v, nil
}
