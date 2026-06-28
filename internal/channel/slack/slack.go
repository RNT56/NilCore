// Package slack drives NilCore over Slack Socket Mode: messages become task
// requests, progress posts back via chat.postMessage, and a gate renders as Block
// Kit Yes/No buttons whose answer feeds policy.Approver (via channel.NewApprover).
// Receiving uses a WebSocket (ws.go, stdlib); sending uses the Web API. Stdlib
// only (invariant I6).
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nilcore/internal/channel"
	"nilcore/internal/eventlog"
)

const defaultAPIBase = "https://slack.com/api"

var retryWait = 2 * time.Second

// event is a decoded Socket Mode envelope.
type event struct {
	Type       string
	EnvelopeID string
	Payload    json.RawMessage
}

// eventSource yields Socket Mode envelopes and acks them. Production uses a
// WebSocket (socketSource); tests inject a fake.
type eventSource interface {
	Next(ctx context.Context) (event, error)
	Ack(ctx context.Context, envelopeID string) error
	Close() error
}

// Bot is a Slack Socket Mode channel. It implements channel.Channel.
type Bot struct {
	appToken string
	botToken string
	apiBase  string
	http     *http.Client

	src     eventSource
	connect func(ctx context.Context) (eventSource, error)
	askSeq  atomic.Int64
	pending []channel.TaskRequest

	mu     sync.Mutex             // guards drafts + asks + pending
	drafts map[string]*slackDraft // per-thread in-place streaming message (chat.update)
	asks   map[string]*askEntry   // ask_user choice prompts awaiting a tap, by token

	authorize func(string) bool // who may answer a gate (nil = anyone; serve sets it)
	log       *eventlog.Log     // for recording rejected gate clicks (may be nil)
}

// slackDraft is the in-place message a thread is currently streaming into: Slack
// has no ephemeral draft, so a "draft" is a real message posted once and edited
// via chat.update. draftID lets a new turn start a fresh message rather than edit
// the finalized prior one.
type slackDraft struct {
	ts      string
	draftID int64
}

var (
	_ channel.Channel       = (*Bot)(nil)
	_ channel.DraftStreamer = (*Bot)(nil)
)

// New returns a bot for the given SLACK_APP_TOKEN (socket) and SLACK_BOT_TOKEN.
func New(appToken, botToken string) *Bot {
	b := &Bot{appToken: appToken, botToken: botToken, apiBase: defaultAPIBase,
		http: &http.Client{Timeout: 30 * time.Second}, drafts: map[string]*slackDraft{}, asks: map[string]*askEntry{}}
	b.connect = b.dialSocket
	return b
}

// SetAuthorizer restricts who may answer an irreversible-action gate: a button
// click from a principal allow rejects is logged and ignored, so a bystander in
// the channel cannot approve a gate (audit H3). With a nil allow (the default)
// any responder is honored — serve always sets this.
func (b *Bot) SetAuthorizer(allow func(string) bool, log *eventlog.Log) {
	b.authorize = allow
	b.log = log
}

// Receive blocks until the next user message, returning it as a task request.
// popPending removes and returns the head of the buffered request queue. The serve
// Receive loop and a backgrounded drive's gate Ask loop can both touch pending, so
// it is mu-guarded (see pushPending).
func (b *Bot) popPending() (channel.TaskRequest, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 {
		return channel.TaskRequest{}, false
	}
	tr := b.pending[0]
	b.pending = b.pending[1:]
	return tr, true
}

// pushPending appends a request to the buffered queue (mu-guarded).
func (b *Bot) pushPending(tr channel.TaskRequest) {
	b.mu.Lock()
	b.pending = append(b.pending, tr)
	b.mu.Unlock()
}

func (b *Bot) Receive(ctx context.Context) (channel.TaskRequest, error) {
	for {
		if tr, ok := b.popPending(); ok {
			return tr, nil
		}
		if err := b.ensure(ctx); err != nil {
			if ctx.Err() != nil {
				return channel.TaskRequest{}, ctx.Err()
			}
			time.Sleep(retryWait)
			continue
		}
		ev, err := b.src.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return channel.TaskRequest{}, ctx.Err()
			}
			b.reset()
			continue
		}
		if ev.EnvelopeID != "" {
			_ = b.src.Ack(ctx, ev.EnvelopeID)
		}
		if ev.Type == "interactive" {
			// An ask_user button tap becomes an ORDINARY authorized task request (the
			// answer line, Sender=clicker) — it flows through the same intake→Permit→
			// Turn→Resolve path a typed message does (I7). A toggle/unauthorized/stale
			// action yields nil.
			if a, ok := askAction(ev.Payload); ok && strings.HasPrefix(a.value, "ask:") {
				if tr := b.handleAskAction(ctx, a); tr != nil {
					return *tr, nil
				}
				continue
			}
		}
		if ev.Type == "events_api" {
			if tr, ok := messageRequest(ev.Payload); ok {
				return tr, nil
			}
		}
	}
}

// Update posts a progress line to the thread (channel).
func (b *Bot) Update(ctx context.Context, threadID, message string) error {
	return b.postMessage(ctx, map[string]any{"channel": threadID, "text": message})
}

// StreamDraft streams a drive's live reasoning into ONE message edited in place:
// the first call for a draftID posts the message (capturing its ts), and each later
// call with the same draftID edits it via chat.update. Slack has no ephemeral
// draft, so the streamed message IS the message; a new draftID starts a fresh one.
// Implements channel.DraftStreamer for serve-mode token streaming.
func (b *Bot) StreamDraft(ctx context.Context, threadID string, draftID int64, text string) error {
	b.mu.Lock()
	cur := b.drafts[threadID]
	b.mu.Unlock()
	if cur == nil || cur.draftID != draftID {
		ts, err := b.postMessageTS(ctx, map[string]any{"channel": threadID, "text": escapeSlack(text)})
		if err != nil {
			return err
		}
		b.mu.Lock()
		b.drafts[threadID] = &slackDraft{ts: ts, draftID: draftID}
		b.mu.Unlock()
		return nil
	}
	return b.updateMessage(ctx, threadID, cur.ts, escapeSlack(text))
}

// FinalizeRich commits the streamed message: it edits the active draft to the final
// text and forgets it (so the next turn posts fresh). With no active draft it
// simply posts the message. Implements channel.DraftStreamer.
func (b *Bot) FinalizeRich(ctx context.Context, threadID, text string) error {
	b.mu.Lock()
	cur := b.drafts[threadID]
	delete(b.drafts, threadID)
	b.mu.Unlock()
	if cur == nil {
		return b.postMessage(ctx, map[string]any{"channel": threadID, "text": escapeSlack(text)})
	}
	return b.updateMessage(ctx, threadID, cur.ts, escapeSlack(text))
}

// postMessageTS posts a message and returns its ts, so later chat.update calls can
// edit it in place (the Slack streaming primitive).
func (b *Bot) postMessageTS(ctx context.Context, body map[string]any) (string, error) {
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		TS    string `json:"ts"`
	}
	if err := b.apiPost(ctx, "chat.postMessage", b.botToken, body, &r); err != nil {
		return "", err
	}
	if !r.OK {
		return "", fmt.Errorf("chat.postMessage: %s", r.Error)
	}
	return r.TS, nil
}

// updateMessage edits a posted message in place via chat.update.
func (b *Bot) updateMessage(ctx context.Context, chanID, ts, text string) error {
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := b.apiPost(ctx, "chat.update", b.botToken, map[string]any{"channel": chanID, "ts": ts, "text": text}, &r); err != nil {
		return err
	}
	if !r.OK {
		return fmt.Errorf("chat.update: %s", r.Error)
	}
	return nil
}

// escapeSlack escapes the three characters Slack treats specially in message text
// (& < >), so arbitrary model prose renders safely.
func escapeSlack(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Ask posts a gate question with Yes/No buttons and blocks for the answer.
func (b *Bot) Ask(ctx context.Context, threadID, question string) (bool, error) {
	id := fmt.Sprintf("ask-%d", b.askSeq.Add(1))
	blocks := []map[string]any{
		{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*GATE* — " + question}},
		{"type": "actions", "elements": []map[string]any{
			{"type": "button", "text": map[string]any{"type": "plain_text", "text": "✅ Yes"}, "value": "yes:" + id, "action_id": "gate_yes", "style": "primary"},
			{"type": "button", "text": map[string]any{"type": "plain_text", "text": "❌ No"}, "value": "no:" + id, "action_id": "gate_no", "style": "danger"},
		}},
	}
	if err := b.postMessage(ctx, map[string]any{"channel": threadID, "text": "GATE — " + question, "blocks": blocks}); err != nil {
		return false, err
	}

	for {
		if err := b.ensure(ctx); err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			time.Sleep(retryWait)
			continue
		}
		ev, err := b.src.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			b.reset()
			continue
		}
		if ev.EnvelopeID != "" {
			_ = b.src.Ack(ctx, ev.EnvelopeID)
		}
		switch ev.Type {
		case "interactive":
			if val, user, ok := blockAction(ev.Payload); ok && strings.HasSuffix(val, ":"+id) {
				if b.authorize != nil && !b.authorize(user) {
					if b.log != nil {
						b.log.Append(eventlog.Event{Kind: "unauthorized_gate",
							Detail: map[string]any{"principal": user, "thread": threadID}})
					}
					continue // ignore; keep waiting for an authorized responder
				}
				return strings.HasPrefix(val, "yes:"), nil
			}
			// An ask_user tap landing in THIS gate's loop must not be dropped: resolve
			// it and buffer the answer task so Receive delivers it.
			if a, ok := askAction(ev.Payload); ok && strings.HasPrefix(a.value, "ask:") {
				if tr := b.handleAskAction(ctx, a); tr != nil {
					b.pushPending(*tr)
				}
			}
		case "events_api":
			if tr, ok := messageRequest(ev.Payload); ok {
				b.pushPending(tr) // don't drop tasks during a gate
			}
		}
	}
}

func (b *Bot) ensure(ctx context.Context) error {
	if b.src != nil {
		return nil
	}
	src, err := b.connect(ctx)
	if err != nil {
		return err
	}
	b.src = src
	return nil
}

func (b *Bot) reset() {
	if b.src != nil {
		_ = b.src.Close()
		b.src = nil
	}
	time.Sleep(retryWait)
}

func messageRequest(payload json.RawMessage) (channel.TaskRequest, bool) {
	var p struct {
		Event struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			User    string `json:"user"`
			Channel string `json:"channel"`
			BotID   string `json:"bot_id"`
			Subtype string `json:"subtype"`
		} `json:"event"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return channel.TaskRequest{}, false
	}
	e := p.Event
	if e.Type != "message" || e.BotID != "" || e.Subtype != "" || strings.TrimSpace(e.Text) == "" {
		return channel.TaskRequest{}, false
	}
	return channel.TaskRequest{Goal: e.Text, Sender: e.User, ThreadID: e.Channel}, true
}

func blockAction(payload json.RawMessage) (value, user string, ok bool) {
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
		return "", "", false
	}
	return p.Actions[0].Value, p.User.ID, true
}

// dialSocket opens a Socket Mode WebSocket via apps.connections.open.
func (b *Bot) dialSocket(ctx context.Context) (eventSource, error) {
	var r struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := b.apiPost(ctx, "apps.connections.open", b.appToken, nil, &r); err != nil {
		return nil, err
	}
	if !r.OK {
		return nil, fmt.Errorf("apps.connections.open: %s", r.Error)
	}
	ws, err := dialWS(ctx, r.URL)
	if err != nil {
		return nil, err
	}
	return &socketSource{ws: ws}, nil
}

func (b *Bot) postMessage(ctx context.Context, body map[string]any) error {
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := b.apiPost(ctx, "chat.postMessage", b.botToken, body, &r); err != nil {
		return err
	}
	if !r.OK {
		return fmt.Errorf("chat.postMessage: %s", r.Error)
	}
	return nil
}

func (b *Bot) apiPost(ctx context.Context, method, token string, body any, out any) error {
	payload := []byte("{}")
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bs
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiBase+"/"+method, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json; charset=utf-8")
	req.Header.Set("authorization", "Bearer "+token)
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack %s: %s", method, resp.Status)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// socketSource reads Socket Mode envelopes off a WebSocket.
type socketSource struct{ ws *wsConn }

func (s *socketSource) Next(context.Context) (event, error) {
	for {
		text, err := s.ws.ReadText()
		if err != nil {
			return event{}, err
		}
		var env struct {
			Type       string          `json:"type"`
			EnvelopeID string          `json:"envelope_id"`
			Payload    json.RawMessage `json:"payload"`
		}
		if json.Unmarshal([]byte(text), &env) != nil || env.Type == "" {
			continue
		}
		return event{Type: env.Type, EnvelopeID: env.EnvelopeID, Payload: env.Payload}, nil
	}
}

func (s *socketSource) Ack(_ context.Context, id string) error {
	if id == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]string{"envelope_id": id})
	return s.ws.WriteText(string(b))
}

func (s *socketSource) Close() error { return s.ws.Close() }
