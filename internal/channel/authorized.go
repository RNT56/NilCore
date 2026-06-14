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

// Ask passes the gate question through to the wrapped channel. Whether an answer
// is honored is decided by GuardedApprove, which the channel layer calls with the
// responding principal.
func (a *Authorized) Ask(ctx context.Context, threadID, question string) (bool, error) {
	return a.Channel.Ask(ctx, threadID, question)
}

// GuardedApprove returns the gate decision only when the responding principal is
// authorized; an unauthorized principal's approval is ignored (treated as a
// denial) and logged. This is the enforcement point the channel routes a gate
// answer through once it knows who clicked.
func (a *Authorized) GuardedApprove(principal string, answer bool) bool {
	if !a.Permit(principal) {
		a.Log.Append(eventlog.Event{Kind: "unauthorized_gate",
			Detail: map[string]any{"principal": principal}})
		return false
	}
	return answer
}
