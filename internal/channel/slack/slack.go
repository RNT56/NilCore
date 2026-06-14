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
	"sync/atomic"
	"time"

	"nilcore/internal/channel"
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
}

var _ channel.Channel = (*Bot)(nil)

// New returns a bot for the given SLACK_APP_TOKEN (socket) and SLACK_BOT_TOKEN.
func New(appToken, botToken string) *Bot {
	b := &Bot{appToken: appToken, botToken: botToken, apiBase: defaultAPIBase, http: &http.Client{Timeout: 30 * time.Second}}
	b.connect = b.dialSocket
	return b
}

// Receive blocks until the next user message, returning it as a task request.
func (b *Bot) Receive(ctx context.Context) (channel.TaskRequest, error) {
	for {
		if len(b.pending) > 0 {
			tr := b.pending[0]
			b.pending = b.pending[1:]
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
			if val, ok := blockAction(ev.Payload); ok && strings.HasSuffix(val, ":"+id) {
				return strings.HasPrefix(val, "yes:"), nil
			}
		case "events_api":
			if tr, ok := messageRequest(ev.Payload); ok {
				b.pending = append(b.pending, tr) // don't drop tasks during a gate
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

func blockAction(payload json.RawMessage) (value string, ok bool) {
	var p struct {
		Type    string `json:"type"`
		Actions []struct {
			Value string `json:"value"`
		} `json:"actions"`
	}
	if json.Unmarshal(payload, &p) != nil || p.Type != "block_actions" || len(p.Actions) == 0 {
		return "", false
	}
	return p.Actions[0].Value, true
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
