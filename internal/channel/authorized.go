package channel

import (
	"context"

	"nilcore/internal/eventlog"
)

// Authorized wraps a Channel with an allowlist of principals permitted to command
// the agent (per-channel user/workspace IDs). The allowlist is empty-by-default
// (deny-all until configured), closing the "anyone who finds the bot drives it"
// hole (docs/OPERATIONS.md §1, P2-T07). Unauthorized inbound commands are rejected
// and logged, never executed; gate approvals are honored only from authorized
// principals.
type Authorized struct {
	Channel Channel
	allowed map[string]struct{}
	Log     *eventlog.Log
}

var _ Channel = (*Authorized)(nil)

// NewAuthorized wraps ch, permitting only the given principals.
func NewAuthorized(ch Channel, allowed []string, log *eventlog.Log) *Authorized {
	set := make(map[string]struct{}, len(allowed))
	for _, p := range allowed {
		if p != "" {
			set[p] = struct{}{}
		}
	}
	return &Authorized{Channel: ch, allowed: set, Log: log}
}

// Permit reports whether a principal may command the agent or approve a gate.
func (a *Authorized) Permit(principal string) bool {
	_, ok := a.allowed[principal]
	return ok
}

// Receive returns only requests from authorized senders. An unauthorized request
// is rejected (logged + the sender told), and Receive keeps waiting — it never
// surfaces an unauthorized command to the orchestrator.
func (a *Authorized) Receive(ctx context.Context) (TaskRequest, error) {
	for {
		req, err := a.Channel.Receive(ctx)
		if err != nil {
			return TaskRequest{}, err
		}
		if a.Permit(req.Sender) {
			return req, nil
		}
		a.Log.Append(eventlog.Event{Kind: "unauthorized_command",
			Detail: map[string]any{"sender": req.Sender, "thread": req.ThreadID}})
		_ = a.Channel.Update(ctx, req.ThreadID, "Unauthorized: you are not permitted to command this agent.")
	}
}

// Update passes progress through to the wrapped channel.
func (a *Authorized) Update(ctx context.Context, threadID, message string) error {
	return a.Channel.Update(ctx, threadID, message)
}

// Ask passes the gate question through to the wrapped channel. Gate-answer
// authorization is NOT enforced here: it lives in the transport, where the clicker
// is known. Each channel's ask handler (slack/telegram choices.go
// handleAskAction/handleAskCallback) authorize()-checks the clicker — emitting the
// same unauthorized_gate audit event and dropping the click — and, because the
// resulting answer rides back in as a TaskRequest whose Sender is the clicker, it is
// re-gated by Permit at server.intake before it can become a principal answer. So an
// answer is never honored from an unauthorized principal, without this wrapper
// needing a separate GuardedApprove seam.
func (a *Authorized) Ask(ctx context.Context, threadID, question string) (bool, error) {
	return a.Channel.Ask(ctx, threadID, question)
}
