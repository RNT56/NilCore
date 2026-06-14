package channel

import (
	"context"

	"nilcore/internal/policy"
)

// chatApprover adapts a Channel + thread into a policy.Approver: an irreversible
// action gate becomes a yes/no chat question via Ask. Used by serve mode to route
// the orchestrator's gate through whatever channel is driving the task. Errors
// (network, cancellation) default-deny — an unanswered gate never proceeds.
type chatApprover struct {
	ctx    context.Context
	ch     Channel
	thread string
}

// NewApprover bridges a Channel to policy.Approver for the given thread.
func NewApprover(ctx context.Context, ch Channel, threadID string) policy.Approver {
	return chatApprover{ctx: ctx, ch: ch, thread: threadID}
}

// Approve asks the human over chat and returns their yes/no (deny on error).
func (a chatApprover) Approve(action string) bool {
	ok, err := a.ch.Ask(a.ctx, a.thread, "Approve this irreversible action?\n"+action)
	if err != nil {
		return false
	}
	return ok
}
