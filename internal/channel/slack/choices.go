package slack

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"nilcore/internal/channel"
	"nilcore/internal/eventlog"
)

var _ channel.ChoicePoster = (*Bot)(nil)

// maxAsks bounds the in-flight ask-prompt registry (an abandoned prompt whose session
// backstop fired without a tap): a new PostChoices over the cap evicts one stale entry.
const maxAsks = 128

// askEntry is one outstanding ask_user choice prompt awaiting a Block Kit button click.
type askEntry struct {
	question string
	choices  []channel.AskChoice
	multi    bool
	picked   []bool
	channel  string // the Slack channel/thread the prompt was posted to
	ts       string // the message ts, for chat.update on a multi-select toggle
}

func (e *askEntry) toggle(i int) {
	if i >= 0 && i < len(e.picked) {
		e.picked[i] = !e.picked[i]
	}
}

// line formats multi-select picks into the resolveReply grammar — INDICES only (never
// label text). Empty when nothing is picked (→ resolveReply's you-decide path).
func (e *askEntry) line() string {
	var idx []string
	for i, p := range e.picked {
		if p {
			idx = append(idx, strconv.Itoa(i+1))
		}
	}
	return strings.Join(idx, ",")
}

// blocks builds the Block Kit message: a section with the question + an actions block of
// buttons. Single-select buttons resolve on click; multi-select buttons toggle (☑/☐) and
// a Done button finalizes. The value carries the correlation token + action.
func (e *askEntry) blocks(token string) []map[string]any {
	var elems []map[string]any
	for i, c := range e.choices {
		label, val := c.Label, "ask:"+token+":"+strconv.Itoa(i)
		if e.multi {
			box := "☐ "
			if i < len(e.picked) && e.picked[i] {
				box = "☑ "
			}
			label, val = box+c.Label, "ask:"+token+":t"+strconv.Itoa(i)
		}
		elems = append(elems, map[string]any{
			"type": "button", "text": map[string]any{"type": "plain_text", "text": label},
			"value": val, "action_id": "ask_" + strconv.Itoa(i),
		})
	}
	if e.multi {
		elems = append(elems, map[string]any{
			"type": "button", "text": map[string]any{"type": "plain_text", "text": "✓ Done"},
			"value": "ask:" + token + ":done", "action_id": "ask_done", "style": "primary",
		})
	}
	hint := "tap a choice, or just type your answer"
	if e.multi {
		hint = "toggle choices then tap Done, or just type your answer"
	}
	return []map[string]any{
		{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*❓ " + e.question + "*"}},
		{"type": "actions", "elements": elems},
		{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": hint}}},
	}
}

// PostChoices renders an ask_user question as Block Kit buttons (channel.ChoicePoster).
func (b *Bot) PostChoices(ctx context.Context, threadID, question string, choices []channel.AskChoice, multiSelect bool) error {
	token := "k" + strconv.FormatInt(b.askSeq.Add(1), 10)
	ent := &askEntry{question: question, choices: choices, multi: multiSelect, picked: make([]bool, len(choices)), channel: threadID}
	ts, err := b.postMessageTS(ctx, map[string]any{"channel": threadID, "text": "❓ " + question, "blocks": ent.blocks(token)})
	if err != nil {
		return err
	}
	ent.ts = ts
	b.mu.Lock()
	if len(b.asks) >= maxAsks {
		for k := range b.asks {
			delete(b.asks, k)
			break
		}
	}
	b.asks[token] = ent
	b.mu.Unlock()
	return nil
}

// askActionPayload is the decoded interesting fields of a Block Kit block_actions event:
// the action value and the clicking user. (The prompt's channel + message ts come from
// the registered askEntry, not the payload, so they are not decoded here.)
type askActionPayload struct {
	value, user string
}

// askAction extracts the ask-button action (value + clicking user) from an interactive
// (block_actions) payload.
func askAction(payload json.RawMessage) (askActionPayload, bool) {
	var p struct {
		Type string `json:"type"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Actions []struct {
			Value string `json:"value"`
		} `json:"actions"`
	}
	if json.Unmarshal(payload, &p) != nil || p.Type != "block_actions" || len(p.Actions) == 0 {
		return askActionPayload{}, false
	}
	return askActionPayload{value: p.Actions[0].Value, user: p.User.ID}, true
}

// handleAskAction turns an "ask:<token>:<action>" click into the authorized answer task,
// or nil for a toggle / unauthorized / stale click. The clicker is authorize()-checked
// here (audit parity with the gate path) AND again at server.intake (Sender=clicker), so
// no click is promoted to a principal answer without passing the same gates (I7).
func (b *Bot) handleAskAction(ctx context.Context, a askActionPayload) *channel.TaskRequest {
	parts := strings.SplitN(a.value, ":", 3)
	if len(parts) != 3 || parts[0] != "ask" {
		return nil
	}
	token, action := parts[1], parts[2]
	b.mu.Lock()
	ent, ok := b.asks[token]
	b.mu.Unlock()
	if !ok {
		return nil // already answered / stale
	}
	if b.authorize != nil && !b.authorize(a.user) {
		if b.log != nil {
			b.log.Append(eventlog.Event{Kind: "unauthorized_gate",
				Detail: map[string]any{"principal": a.user, "thread": ent.channel, "kind": "ask"}})
		}
		return nil
	}
	switch {
	case action == "done":
		line := ent.line()
		b.consume(token)
		return &channel.TaskRequest{Goal: line, Sender: a.user, ThreadID: ent.channel}
	case strings.HasPrefix(action, "t"):
		if i, err := strconv.Atoi(action[1:]); err == nil {
			ent.toggle(i)
			b.updateBlocks(ctx, ent, token)
		}
		return nil
	default:
		i, err := strconv.Atoi(action)
		if err != nil {
			return nil
		}
		b.consume(token)
		return &channel.TaskRequest{Goal: strconv.Itoa(i + 1), Sender: a.user, ThreadID: ent.channel}
	}
}

func (b *Bot) consume(token string) {
	b.mu.Lock()
	delete(b.asks, token)
	b.mu.Unlock()
}

// updateBlocks re-renders the prompt's buttons in place (chat.update) after a toggle so
// the operator sees the ☑ checkmarks accumulate.
func (b *Bot) updateBlocks(ctx context.Context, ent *askEntry, token string) {
	_ = b.apiPost(ctx, "chat.update", b.botToken, map[string]any{
		"channel": ent.channel, "ts": ent.ts, "text": "❓ " + ent.question, "blocks": ent.blocks(token),
	}, nil)
}
