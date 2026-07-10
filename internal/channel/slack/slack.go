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
	"strconv"
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

	mu            sync.Mutex             // guards src, pending, drafts, asks, gates, intakeStarted
	drafts        map[string]*slackDraft // per-thread in-place streaming message (chat.update)
	asks          map[string]*askEntry   // ask_user choice prompts awaiting a tap, by token
	gates         map[string]*gateEntry  // pending gate id -> answer waiter (the intake routes here)
	intakeStarted bool                   // the single socket-owner goroutine is running

	// taskWake is poked (buffered, non-blocking) after a task is queued so a parked
	// Receive re-checks the queue. Buffered(1): the intake never blocks on it.
	taskWake chan struct{}

	authorize func(string) bool // who may answer a gate (nil = anyone; serve sets it)
	log       *eventlog.Log     // for recording rejected gate clicks (may be nil)
}

// gateEntry is one pending Ask awaiting its yes/no answer. reply is buffered(1) so the
// intake delivers the first answer without ever blocking; thread is kept for the
// unauthorized-click audit line.
type gateEntry struct {
	reply  chan bool
	thread string
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
		http: &http.Client{Timeout: 30 * time.Second}, drafts: map[string]*slackDraft{},
		asks: map[string]*askEntry{}, gates: map[string]*gateEntry{}, taskWake: make(chan struct{}, 1)}
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

// pushPending appends a request to the buffered queue (mu-guarded) and pokes taskWake
// so a parked Receive re-checks. The poke is non-blocking (buffered 1) so the single
// intake goroutine never stalls on it.
func (b *Bot) pushPending(tr channel.TaskRequest) {
	b.mu.Lock()
	b.pending = append(b.pending, tr)
	b.mu.Unlock()
	select {
	case b.taskWake <- struct{}{}:
	default:
	}
}

// Receive blocks until the next task request arrives. It NEVER reads the socket itself:
// the single intake goroutine (startIntake) owns the socket and queues task requests,
// and Receive drains that queue. That is what removes the Receive/Ask data race over the
// one WebSocket (a gate's Ask and the serve Receive loop run concurrently).
func (b *Bot) Receive(ctx context.Context) (channel.TaskRequest, error) {
	b.startIntake(ctx)
	for {
		if tr, ok := b.popPending(); ok {
			return tr, nil
		}
		select {
		case <-b.taskWake:
		case <-ctx.Done():
			return channel.TaskRequest{}, ctx.Err()
		}
	}
}

// Update posts a progress line to the thread (channel). The message is model- and
// tool-derived (a serve surface line, an echoed goal) so it is escaped: untrusted
// content must never carry active Slack markup like <!channel> or <http://x|y> (I7).
func (b *Bot) Update(ctx context.Context, threadID, message string) error {
	return b.postMessage(ctx, map[string]any{"channel": threadID, "text": escapeSlack(message)})
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
// and mrkdwn (& < >), so arbitrary model/tool prose renders as inert DATA. This is a
// security control, not cosmetics: a raw '<' opens Slack's special-sequence syntax, so
// untrusted content like <!channel> / <!here> (mass pings), <@U123> (user mentions), or
// <http://evil|innocent> (forged links) would otherwise be INTERPRETED by Slack when it
// reaches a text/mrkdwn field. Escaping '<' (and '&', to keep entity text literal)
// neutralizes all of them (I7). Button labels use plain_text and are not interpreted, so
// they do not need this.
func escapeSlack(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// slackSectionLimit is Slack's Block Kit section `text` character cap. Text beyond it
// makes chat.postMessage reject the WHOLE message with invalid_blocks — so an
// evidence-rich gate (the compact evidence appendix is sized for Telegram's 4096 cap)
// would error, Ask would return that error, and the structured approver would then
// default-DENY a gate the operator never saw. Clipping the section to this limit keeps
// the gate renderable and shown; the full evidence still lives on the terminal/event log.
const slackSectionLimit = 3000

// clipRunes bounds s to at most max runes, cutting on a rune boundary (so the result is
// always valid UTF-8) and marking truncation with an ellipsis. Used to fit a section
// under slackSectionLimit; Slack mrkdwn is lenient, so a severed entity renders oddly at
// worst — never a hard parse error like Telegram's MarkdownV2.
func clipRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

// Ask posts a gate question with Yes/No buttons and blocks for the answer.
func (b *Bot) Ask(ctx context.Context, threadID, question string) (bool, error) {
	id := fmt.Sprintf("ask-%d", b.askSeq.Add(1))
	reply := make(chan bool, 1)
	// Register the gate BEFORE the intake could ever read its answer (and before posting
	// the question), so a fast answer is always routed to a registered waiter.
	b.registerGate(id, threadID, reply)
	defer b.unregisterGate(id)
	b.startIntake(ctx)

	// The question is model-derived and may fold untrusted repo/tool content, so it is
	// escaped (I7); the "*GATE* —" prefix is harness-controlled bold and stays literal.
	// The section is then clipped to Slack's 3000-char cap so an evidence-rich gate
	// renders instead of erroring into a silent default-deny.
	section := clipRunes("*GATE* — "+escapeSlack(question), slackSectionLimit)
	fallback := clipRunes("GATE — "+escapeSlack(question), slackSectionLimit)
	blocks := []map[string]any{
		{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": section}},
		{"type": "actions", "elements": []map[string]any{
			{"type": "button", "text": map[string]any{"type": "plain_text", "text": "✅ Yes"}, "value": "yes:" + id, "action_id": "gate_yes", "style": "primary"},
			{"type": "button", "text": map[string]any{"type": "plain_text", "text": "❌ No"}, "value": "no:" + id, "action_id": "gate_no", "style": "danger"},
		}},
	}
	if err := b.postMessage(ctx, map[string]any{"channel": threadID, "text": fallback, "blocks": blocks}); err != nil {
		return false, err
	}
	select {
	case ans := <-reply:
		return ans, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// startIntake launches the SINGLE socket-owner goroutine once, for the lifetime of the
// first caller's ctx — in serve that is the long-lived Receive loop. Exactly one
// goroutine ever reads the socket, so Receive and any number of concurrent Asks never
// race over it.
func (b *Bot) startIntake(ctx context.Context) {
	b.mu.Lock()
	start := !b.intakeStarted
	if start {
		b.intakeStarted = true
	}
	b.mu.Unlock()
	if start {
		go b.intake(ctx)
	}
}

// intake is the sole socket reader: it pulls each Socket Mode envelope and routes it —
// a gate answer to the waiting Ask, an ask_user tap or a message to the task queue.
//
// On exit (its ctx cancelled) it clears intakeStarted so the NEXT startIntake spins a
// fresh reader — the property (mirrored from telegram, pinned by a restart test) that
// makes a per-drive starter unable to wedge intake: if a gate Ask were ever the first
// startIntake caller and its per-drive ctx ended, the socket owner would die AND unlatch,
// so a later Receive revives it. Without this clear the flag stayed true forever and
// Receive would block on taskWake with no reader — a permanent wedge.
func (b *Bot) intake(ctx context.Context) {
	defer func() {
		b.mu.Lock()
		b.intakeStarted = false
		b.mu.Unlock()
	}()
	for {
		if err := b.ensureConn(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(retryWait)
			continue
		}
		src := b.currentSrc()
		if src == nil {
			continue
		}
		ev, err := src.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.reset()
			continue
		}
		if ev.EnvelopeID != "" {
			_ = src.Ack(ctx, ev.EnvelopeID)
		}
		b.route(ctx, ev)
	}
}

// route classifies one envelope. A gate button answer for a PENDING gate is delivered to
// that gate's waiter (after the authorizer check); an ask_user tap or a message becomes a
// task request. A stale/foreign action is dropped.
func (b *Bot) route(ctx context.Context, ev event) {
	switch ev.Type {
	case "interactive":
		if val, user, ok := blockAction(ev.Payload); ok {
			if g := b.lookupGate(gateID(val)); g != nil {
				if b.authorize != nil && !b.authorize(user) {
					if b.log != nil {
						b.log.Append(eventlog.Event{Kind: "unauthorized_gate",
							Detail: map[string]any{"principal": user, "thread": g.thread}})
					}
					return // ignore; the gate keeps waiting for an authorized responder
				}
				select {
				case g.reply <- strings.HasPrefix(val, "yes:"):
				default: // already answered (buffered 1) — first answer wins
				}
				return
			}
		}
		// Not a gate answer: an ask_user tap becomes an ORDINARY authorized task request
		// (the answer line, Sender=clicker), flowing through the same intake→Permit→Turn
		// path a typed message does (I7).
		if a, ok := askAction(ev.Payload); ok && strings.HasPrefix(a.value, "ask:") {
			if tr := b.handleAskAction(ctx, a); tr != nil {
				b.pushPending(*tr)
			}
		}
	case "events_api":
		if tr, ok := messageRequest(ev.Payload); ok {
			b.pushPending(tr)
		}
	}
}

// gateID extracts the gate id from a button value ("yes:ask-3" / "no:ask-3" → "ask-3").
func gateID(val string) string {
	if i := strings.IndexByte(val, ':'); i >= 0 {
		return val[i+1:]
	}
	return ""
}

func (b *Bot) registerGate(id, thread string, reply chan bool) {
	b.mu.Lock()
	b.gates[id] = &gateEntry{reply: reply, thread: thread}
	b.mu.Unlock()
}

func (b *Bot) unregisterGate(id string) {
	b.mu.Lock()
	delete(b.gates, id)
	b.mu.Unlock()
}

func (b *Bot) lookupGate(id string) *gateEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gates[id]
}

// ensureConn connects the socket if needed (mu-guarded; only the intake calls it).
func (b *Bot) ensureConn(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
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

func (b *Bot) currentSrc() eventSource {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.src
}

func (b *Bot) reset() {
	b.mu.Lock()
	if b.src != nil {
		_ = b.src.Close()
		b.src = nil
	}
	b.mu.Unlock()
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
	// Bounded retry on HTTP 429: Slack's Web API rate-limits (chat.update streaming can
	// exceed the Tier-3 budget), and a dropped gate/ask/final message must not be silently
	// lost. Honor Retry-After, cap the wait, and give up after maxRateRetries — never loop
	// unboundedly. The payload bytes are reusable, so each attempt rebuilds its own request.
	for attempt := 0; ; attempt++ {
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
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRateRetries {
			if err := sleepCtx(ctx, retryAfter(resp.Header.Get("Retry-After"))); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("slack %s: %s", method, resp.Status)
		}
		if out != nil {
			return json.Unmarshal(raw, out)
		}
		return nil
	}
}

// Rate-limit backoff knobs. maxRateRetries bounds the retry count; the wait is derived
// from Retry-After, floored at rateRetryDefault and capped at rateRetryMax so a hostile
// or absurd value can never park a call for long. rateRetryDefault is a var only so tests
// can shrink it.
const (
	maxRateRetries = 3
	rateRetryMax   = 30 * time.Second
)

var rateRetryDefault = 1 * time.Second

// retryAfter parses a Retry-After header (delta-seconds) into a bounded wait: a
// missing/garbage value falls back to rateRetryDefault, and the result is capped at
// rateRetryMax.
func retryAfter(h string) time.Duration {
	d := rateRetryDefault
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n > 0 {
		d = time.Duration(n) * time.Second
	}
	if d > rateRetryMax {
		d = rateRetryMax
	}
	return d
}

// sleepCtx sleeps for d, returning early (with its error) if ctx is cancelled — so a
// backoff never outlives the request's context.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
