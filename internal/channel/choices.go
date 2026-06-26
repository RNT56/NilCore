package channel

import "context"

// choices.go adds NATIVE choice rendering for the ask_user feature as an OPTIONAL
// transport capability — exactly the DraftStreamer pattern (stream.go), so the FROZEN
// channel.go contract (Receive/Update/Ask) is never touched (CLAUDE.md §5 + I1). A
// transport that implements ChoicePoster renders an ask_user question as tappable
// buttons (Telegram inline_keyboard, Slack Block Kit); one that does not falls back to
// a plain "❓ "+text Update and a typed reply, byte-identical to today.
//
// The answer round-trip preserves I7 with ZERO new trust seams: a button tap is turned
// by the transport into an ordinary channel.TaskRequest whose Sender is the CLICKER —
// it then flows through the SAME authorized path every typed message does (Receive →
// server.intake → Auth.Permit → the thread's sender-pin → Session.Turn → askBox.Resolve
// → resolveReply). A tap is just a faster, less error-prone way to type "1,3"; the
// transport formats only INDICES into the line grammar (never label text, which may
// contain ',' or ';'), so resolveReply stays the single answer parser.

// AskChoice is one labelled option to render as a button (a channel-local mirror of the
// emit/backend choice types — channel imports neither, staying an independent leaf).
type AskChoice struct {
	Label, Detail string
}

// ChoicePoster is the optional capability a transport implements to render an ask_user
// question as native buttons. PostChoices posts the question + choices to a thread; the
// transport owns the per-prompt correlation state so a later tap maps back to the right
// answer line and is delivered as an authorized TaskRequest from Receive. multiSelect
// renders toggle buttons plus a "Done" action; single-select resolves on the first tap.
// A free-text reply is always still accepted (the typed-answer fallback).
type ChoicePoster interface {
	PostChoices(ctx context.Context, threadID, question string, choices []AskChoice, multiSelect bool) error
}
