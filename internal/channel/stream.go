package channel

import "context"

// DraftStreamer is an OPTIONAL capability a Channel transport may implement to
// stream a drive's live reasoning as an in-place, ephemeral draft and then
// persist a final, richly-formatted message. It is additive: the serve sink
// type-asserts a Channel for it and falls back to plain Update when it is absent,
// so a transport that does not implement it is unchanged (the Channel contract in
// channel.go is untouched).
//
//   - StreamDraft updates the ephemeral draft on a thread; successive calls with
//     the same non-zero draftID animate smoothly in place (Telegram
//     sendMessageDraft — a 30-second preview the operator sees being "typed"). The
//     text is PLAIN: partial rich markup mid-stream would be invalid, so the
//     stream carries plain tokens and FinalizeRich applies markup once.
//   - FinalizeRich persists the completed message in the transport's native rich
//     markup (Telegram MarkdownV2, Slack mrkdwn), replacing the ephemeral draft.
type DraftStreamer interface {
	StreamDraft(ctx context.Context, threadID string, draftID int64, text string) error
	FinalizeRich(ctx context.Context, threadID string, richText string) error
}
