package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"nilcore/internal/channel"
	"nilcore/internal/eventlog"
)

var _ channel.ChoicePoster = (*Bot)(nil)

// maxAsks bounds the in-flight ask-prompt registry so an abandoned prompt (one whose
// session-side wall-clock backstop fired without a tap) cannot leak unboundedly: a new
// PostChoices over the cap evicts an arbitrary stale entry. A consumed (answered) prompt
// is deleted immediately, so the cap only ever trims truly-abandoned prompts.
const maxAsks = 128

// askEntry is one outstanding ask_user choice prompt awaiting a tap. picked accumulates
// multi-select toggles; msgID lets a toggle re-render the keyboard in place.
type askEntry struct {
	choices []channel.AskChoice
	multi   bool
	mu      sync.Mutex // guards picked (concurrent taps can land in either poll loop)
	picked  []bool
	chatID  int64
	msgID   int64
}

func (e *askEntry) toggle(i int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i >= 0 && i < len(e.picked) {
		e.picked[i] = !e.picked[i]
	}
}

// line formats the multi-select picks into the resolveReply grammar — INDICES ONLY
// (never label text, which may contain ',' or ';'), so the answer parser stays the sole
// authority. Empty when nothing is picked (→ resolveReply's you-decide path).
func (e *askEntry) line() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var idx []string
	for i, p := range e.picked {
		if p {
			idx = append(idx, strconv.Itoa(i+1))
		}
	}
	return strings.Join(idx, ",")
}

// keyboard builds the inline_keyboard for this prompt. Single-select: one button per
// choice (a tap resolves immediately). Multi-select: a toggle button per choice (☑/☐)
// plus a Done button that finalizes the accumulated picks. The callback_data carries the
// correlation token so a tap maps back to this prompt regardless of which poll loop sees it.
func (e *askEntry) keyboard(token string) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	var rows [][]map[string]any
	for i, c := range e.choices {
		label, data := c.Label, "ask:"+token+":"+strconv.Itoa(i)
		if e.multi {
			box := "☐ "
			if i < len(e.picked) && e.picked[i] {
				box = "☑ "
			}
			label, data = box+c.Label, "ask:"+token+":t"+strconv.Itoa(i)
		}
		rows = append(rows, []map[string]any{{"text": label, "callback_data": data}})
	}
	if e.multi {
		rows = append(rows, []map[string]any{{"text": "✓ Done", "callback_data": "ask:" + token + ":done"}})
	}
	return map[string]any{"inline_keyboard": rows}
}

// PostChoices renders an ask_user question as native inline buttons (channel.ChoicePoster).
// The thread can still answer by typing — the buttons are a faster, less error-prone way
// to produce the same answer line. The correlation token round-trips in callback_data.
func (b *Bot) PostChoices(ctx context.Context, threadID, question string, choices []channel.AskChoice, multiSelect bool) error {
	chatID, err := strconv.ParseInt(threadID, 10, 64)
	if err != nil {
		return fmt.Errorf("bad thread id %q: %w", threadID, err)
	}
	token := "k" + strconv.FormatInt(b.askSeq.Add(1), 10)
	ent := &askEntry{choices: choices, multi: multiSelect, picked: make([]bool, len(choices)), chatID: chatID}
	hint := "\n(tap a choice, or just type your answer)"
	if multiSelect {
		hint = "\n(toggle choices then tap Done, or type your answer)"
	}
	var resp struct {
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := b.call(ctx, "sendMessage", map[string]any{
		"chat_id": chatID, "text": "❓ " + question + hint, "reply_markup": ent.keyboard(token),
	}, &resp); err != nil {
		return err
	}
	ent.msgID = resp.Result.MessageID
	b.amu.Lock()
	if len(b.asks) >= maxAsks {
		for k := range b.asks { // evict one arbitrary abandoned prompt to bound the map
			delete(b.asks, k)
			break
		}
	}
	b.asks[token] = ent
	b.amu.Unlock()
	return nil
}

// handleAskCallback turns an "ask:<token>:<action>" button tap into the authorized
// answer task, or nil for a toggle / unauthorized / stale tap. The clicker is
// authorize()-checked HERE (audit parity with the gate-tap path) AND, because the
// returned TaskRequest's Sender is the clicker, again at server.intake's Auth.Permit and
// the thread's sender-pin — a tap is never promoted to a principal answer without passing
// the same gates a typed message does (I7).
func (b *Bot) handleAskCallback(ctx context.Context, cb *tgCallback) *channel.TaskRequest {
	parts := strings.SplitN(cb.Data, ":", 3)
	if len(parts) != 3 || parts[0] != "ask" {
		return nil
	}
	token, action := parts[1], parts[2]
	b.amu.Lock()
	ent, ok := b.asks[token]
	b.amu.Unlock()
	if !ok {
		_ = b.answerCallback(ctx, cb.ID, "this question is already answered")
		return nil
	}
	clicker := strconv.FormatInt(cb.From.ID, 10)
	if b.authorize != nil && !b.authorize(clicker) {
		if b.log != nil {
			b.log.Append(eventlog.Event{Kind: "unauthorized_gate",
				Detail: map[string]any{"principal": clicker, "thread": strconv.FormatInt(cb.Message.Chat.ID, 10), "kind": "ask"}})
		}
		_ = b.answerCallback(ctx, cb.ID, "Not authorized to answer this.")
		return nil
	}
	threadID := strconv.FormatInt(cb.Message.Chat.ID, 10)
	switch {
	case action == "done": // multi-select finalize
		claimed, ok := b.claim(token) // atomic lookup+delete: only the first finalize wins
		if !ok {
			_ = b.answerCallback(ctx, cb.ID, "this question is already answered")
			return nil
		}
		_ = b.answerCallback(ctx, cb.ID, "✓")
		return &channel.TaskRequest{Goal: claimed.line(), Sender: clicker, ThreadID: threadID}
	case strings.HasPrefix(action, "t"): // multi-select toggle: update + re-render, no answer yet
		if i, err := strconv.Atoi(action[1:]); err == nil {
			ent.toggle(i)
			_ = b.call(ctx, "editMessageReplyMarkup", map[string]any{
				"chat_id": ent.chatID, "message_id": ent.msgID, "reply_markup": ent.keyboard(token),
			}, nil)
		}
		_ = b.answerCallback(ctx, cb.ID, "")
		return nil
	default: // single-select tap "<i>"
		i, err := strconv.Atoi(action)
		if err != nil {
			_ = b.answerCallback(ctx, cb.ID, "")
			return nil
		}
		if _, ok := b.claim(token); !ok { // atomic: a second concurrent tap gets nothing
			_ = b.answerCallback(ctx, cb.ID, "this question is already answered")
			return nil
		}
		_ = b.answerCallback(ctx, cb.ID, "✓")
		return &channel.TaskRequest{Goal: strconv.Itoa(i + 1), Sender: clicker, ThreadID: threadID}
	}
}

// claim atomically looks up AND removes a pending prompt, so only the FIRST terminal tap
// for a token produces an answer (a concurrent second tap gets ok=false). This closes the
// lookup→consume TOCTOU that could otherwise double-resolve a single question.
func (b *Bot) claim(token string) (*askEntry, bool) {
	b.amu.Lock()
	defer b.amu.Unlock()
	ent, ok := b.asks[token]
	if ok {
		delete(b.asks, token)
	}
	return ent, ok
}

func (b *Bot) answerCallback(ctx context.Context, id, text string) error {
	return b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}
