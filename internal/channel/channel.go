// Package channel defines the one seam every chat transport implements so a
// human can drive NilCore from a phone (CLAUDE.md §5 contract). It is minimal
// and transport-agnostic: a channel delivers task requests, streams progress
// back, and renders an irreversible-action gate as a yes/no the human answers in
// chat. Concrete transports (Telegram, Slack) live in internal/channel/<name>;
// authorization of senders is P2-T07.
package channel

import "context"

// TaskRequest is an inbound request to run a coding task.
type TaskRequest struct {
	Goal     string // the task, in plain language
	Sender   string // principal id of the requester (Phase-2 allowlist, P2-T07)
	ThreadID string // opaque conversation id used to route replies back
}

// Channel is the transport seam. Implementations block on Receive for the next
// request, stream human-readable progress via Update, and pose gate questions via
// Ask — the chat form of policy.Approver. All methods honor ctx cancellation.
type Channel interface {
	// Receive blocks until the next task request arrives or ctx is cancelled.
	Receive(ctx context.Context) (TaskRequest, error)

	// Update sends a progress line to the request's thread.
	Update(ctx context.Context, threadID, message string) error

	// Ask poses an irreversible-action gate question to the thread and blocks
	// until the human answers yes (true) or no (false).
	Ask(ctx context.Context, threadID, question string) (bool, error)
}
