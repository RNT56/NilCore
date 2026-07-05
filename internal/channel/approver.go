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

// compactEvidenceLimit bounds the evidence appendix on a chat gate question so
// the whole message stays inside transport caps (Telegram hard-caps a message at
// 4096 chars; the question header and action line use the rest). Channels get
// the diffstat + verify tail + spend only — the full diff excerpt deliberately
// stays on the terminal/TUI surfaces and in the event log, and the compact form
// says so.
const compactEvidenceLimit = 3400

// ApproveStructured is the evidence-aware gate (policy.StructuredApprover): the
// same question, plus a bounded diffstat / verify-tail / spend appendix when the
// action carries evidence. The appendix is DATA rendered to the human (I7) —
// already redacted and bounded at construction — and with no evidence the
// message is byte-identical to Approve's (pinned by test), so unaware gate sites
// and evidence-less actions are unchanged.
func (a chatApprover) ApproveStructured(act policy.GateAction) bool {
	q := "Approve this irreversible action?\n" + act.Describe()
	if s := act.Evidence.RenderCompact(compactEvidenceLimit); s != "" {
		q += "\n\n" + s
	}
	ok, err := a.ch.Ask(a.ctx, a.thread, q)
	if err != nil {
		return false
	}
	return ok
}
