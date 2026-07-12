// Package telegram drives NilCore from a phone over the Telegram Bot API: text
// messages become task requests, progress streams back as messages, and an
// irreversible-action gate renders as inline Yes/No buttons whose answer feeds
// policy.Approver (via channel.NewApprover). Stdlib HTTP only (invariant I6).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

const defaultAPIBase = "https://api.telegram.org"

// retryWait is how long to pause after a transient poll error before retrying.
var retryWait = 2 * time.Second

// Bot is a long-polling Telegram channel. It implements channel.Channel.
type Bot struct {
	token   string
	baseURL string
	http    *http.Client

	// smu guards offset + pending. In serve mode the Receive loop and a backgrounded
	// drive's gate Ask loop can both call poll() (advancing offset) and both touch
	// pending concurrently — so these need synchronization (the network call in poll
	// runs OUTSIDE the lock). Kept separate from amu to avoid re-entrancy with
	// handleAskCallback, which locks amu.
	smu           sync.Mutex
	offset        int                   // last seen update_id (guarded by smu)
	askSeq        atomic.Int64          // unique gate/ask-callback ids
	pending       []channel.TaskRequest // task messages (and resolved ask taps) (guarded by smu)
	gates         map[string]*gateEntry // pending gate id -> answer waiter (guarded by smu; the poller routes here)
	intakeStarted bool                  // the single poller goroutine is running (guarded by smu)
	// taskWake is poked (buffered, non-blocking) after a task is queued so a parked
	// Receive re-checks the queue. Buffered(1): the single poller never blocks on it.
	taskWake chan struct{}

	amu  sync.Mutex           // guards asks
	asks map[string]*askEntry // ask_user choice prompts awaiting a tap, by correlation token

	authorize func(string) bool // who may answer a gate / ask (nil = anyone; serve sets it)
	log       *eventlog.Log     // for recording rejected gate/ask clicks (may be nil)
}

// gateEntry is one pending Ask awaiting its yes/no answer. reply is buffered(1) so the
// single poller delivers the first answer without ever blocking; thread is kept for the
// unauthorized-click audit line.
type gateEntry struct {
	reply  chan bool
	thread string
}

var _ channel.Channel = (*Bot)(nil)

// New returns a bot for the given TELEGRAM_BOT_TOKEN.
func New(token string) *Bot {
	return &Bot{
		token:    token,
		baseURL:  defaultAPIBase,
		asks:     map[string]*askEntry{},
		gates:    map[string]*gateEntry{},
		taskWake: make(chan struct{}, 1),
		// Slightly longer than the long-poll timeout below.
		http: &http.Client{Timeout: 70 * time.Second},
	}
}

// SetAuthorizer restricts who may answer an irreversible-action gate: a button
// click from a principal allow rejects is logged and ignored, so a bystander who
// can see the chat cannot approve a gate (audit H3). With a nil allow (the
// default) any responder is honored — serve always sets this.
func (b *Bot) SetAuthorizer(allow func(string) bool, log *eventlog.Log) {
	b.authorize = allow
	b.log = log
}

type tgMessage struct {
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	Text string `json:"text"`
}

type tgCallback struct {
	ID   string `json:"id"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Message struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
	Data string `json:"data"`
}

type tgUpdate struct {
	UpdateID      int         `json:"update_id"`
	Message       *tgMessage  `json:"message"`
	CallbackQuery *tgCallback `json:"callback_query"`
}

type updatesResp struct {
	OK     bool       `json:"ok"`
	Result []tgUpdate `json:"result"`
}

// popPending removes and returns the head of the buffered request queue (smu-guarded).
func (b *Bot) popPending() (channel.TaskRequest, bool) {
	b.smu.Lock()
	defer b.smu.Unlock()
	if len(b.pending) == 0 {
		return channel.TaskRequest{}, false
	}
	tr := b.pending[0]
	b.pending = b.pending[1:]
	return tr, true
}

// pushPending appends a request to the buffered queue (smu-guarded) and pokes taskWake
// so a parked Receive re-checks. The poke is non-blocking (buffered 1) so the single
// poller goroutine never stalls on it.
func (b *Bot) pushPending(tr channel.TaskRequest) {
	b.smu.Lock()
	b.pending = append(b.pending, tr)
	b.smu.Unlock()
	select {
	case b.taskWake <- struct{}{}:
	default:
	}
}

// Receive blocks until the next task request arrives. It NEVER polls itself: the single
// poller goroutine (startIntake) owns the offset + getUpdates and queues task requests,
// and Receive drains that queue. That removes the Receive/Ask double-delivery race —
// previously two concurrent poll() calls read the same offset and each advanced it,
// delivering every update twice.
func (b *Bot) Receive(ctx context.Context) (channel.TaskRequest, error) {
	// In serve, Receive runs first with the long-lived serve ctx and owns the poller's
	// lifetime. If a gate Ask started (and then lost) the poller under a per-drive ctx
	// first, startIntake is restartable, so this call revives it under the serve ctx.
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

// Update sends a progress line to the thread (chat). The text is plain (no parse_mode)
// but still clipped to the Bot API cap: a >4096-char progress line is otherwise rejected
// with HTTP 400 and the whole update is dropped.
func (b *Bot) Update(ctx context.Context, threadID, message string) error {
	chatID, err := strconv.ParseInt(threadID, 10, 64)
	if err != nil {
		return fmt.Errorf("bad thread id %q: %w", threadID, err)
	}
	return b.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": clipText(message)}, nil)
}

// telegramTextLimit is the Bot API per-message character cap (after entity parsing).
const telegramTextLimit = 4096

var _ channel.DraftStreamer = (*Bot)(nil)

// StreamDraft updates the ephemeral, in-place draft for a drive's live reasoning
// via sendMessageDraft (Bot API 9.5+): successive calls with the same non-zero
// draftID animate the draft smoothly — a 30-second preview the operator sees being
// "typed". The text is PLAIN (no parse_mode): partial MarkdownV2 mid-stream would
// be invalid, so the stream carries plain tokens and FinalizeRich applies markup
// once. Draft streaming is a private-chat feature; on a non-private thread the API
// errors and the serve sink falls back to Update.
func (b *Bot) StreamDraft(ctx context.Context, threadID string, draftID int64, text string) error {
	chatID, err := strconv.ParseInt(threadID, 10, 64)
	if err != nil {
		return fmt.Errorf("bad thread id %q: %w", threadID, err)
	}
	if draftID == 0 {
		draftID = 1 // the API requires a non-zero draft_id
	}
	return b.call(ctx, "sendMessageDraft", map[string]any{
		"chat_id": chatID, "draft_id": draftID, "text": clipText(text),
	}, nil)
}

// FinalizeRich persists the completed message to the thread (replacing the
// ephemeral draft) in MarkdownV2 mode. text is PLAIN — it is escaped here so
// arbitrary model prose renders safely without the sink (which is generic over
// channel.Channel) needing to know any transport-specific markup; structural
// markup is a future enhancement layered on top. Sending a normal message is what
// finalizes a draft (Bot API), so this both renders and commits.
func (b *Bot) FinalizeRich(ctx context.Context, threadID, text string) error {
	chatID, err := strconv.ParseInt(threadID, 10, 64)
	if err != nil {
		return fmt.Errorf("bad thread id %q: %w", threadID, err)
	}
	return b.call(ctx, "sendMessage", map[string]any{
		"chat_id": chatID, "text": clipEscapeMarkdownV2(text), "parse_mode": "MarkdownV2",
	}, nil)
}

// markdownV2Special is the set of MarkdownV2 reserved characters that must be
// backslash-escaped in body text. It INCLUDES the backslash itself: MarkdownV2 uses
// '\' as its escape char, so a literal '\' in model prose (a Windows path, a regex
// like \d+) must become '\\' or Telegram rejects the message with "can't parse
// entities".
const markdownV2Special = "\\_*[]()~`>#+-=|{}.!"

// EscapeMarkdownV2 escapes the MarkdownV2 reserved characters so arbitrary text
// (model output, a tool name) is safe inside a MarkdownV2 message without breaking
// its formatting. The renderer escapes the text PARTS and wraps them in its own
// markup (* _ ` >).
func EscapeMarkdownV2(s string) string {
	var out strings.Builder
	out.Grow(len(s) + 8)
	for _, r := range s {
		if strings.ContainsRune(markdownV2Special, r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

// clipEscapeMarkdownV2 escapes s for MarkdownV2 AND bounds the RESULT to the Bot
// API character cap, cutting only on whole escape pairs. Escaping must precede a
// length check (it can nearly double the length), but clipping the escaped string
// blindly can sever a "\x" pair and leave a dangling backslash — an invalid escape
// Telegram rejects, losing the whole finalized message. So this escapes rune by
// rune, stops before a pair would breach the cap (reserving one rune for the
// ellipsis, itself non-reserved so it stays valid), and never emits a lone '\'.
func clipEscapeMarkdownV2(s string) string {
	var out strings.Builder
	out.Grow(len(s) + 8)
	count := 0
	for _, r := range s {
		w := 1
		if strings.ContainsRune(markdownV2Special, r) {
			w = 2 // a reserved char emits "\<r>", two runes
		}
		if count+w > telegramTextLimit-1 { // keep one rune of headroom for the ellipsis
			out.WriteString("…")
			return out.String()
		}
		if w == 2 {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
		count += w
	}
	return out.String()
}

// clipText bounds a PLAIN message (no escaping) to the Bot API character cap,
// cutting on a rune boundary so a clipped message never carries invalid UTF-8. Used
// by the plain draft stream; the rich finalize uses clipEscapeMarkdownV2.
func clipText(s string) string {
	r := []rune(s)
	if len(r) <= telegramTextLimit {
		return s
	}
	return string(r[:telegramTextLimit-1]) + "…"
}

// Ask poses a gate question with inline Yes/No buttons and blocks for the answer.
func (b *Bot) Ask(ctx context.Context, threadID, question string) (bool, error) {
	chatID, err := strconv.ParseInt(threadID, 10, 64)
	if err != nil {
		return false, fmt.Errorf("bad thread id %q: %w", threadID, err)
	}
	id := fmt.Sprintf("ask-%d", b.askSeq.Add(1))
	reply := make(chan bool, 1)
	// Register the gate BEFORE the poller could read its answer (and before posting),
	// so a fast answer is always routed to a registered waiter.
	b.registerGate(id, threadID, reply)
	defer b.unregisterGate(id)
	// Ask ensures the poller is up so its gate answer is delivered. If Ask is the first
	// caller (its ctx is a per-drive ctx via channel.Approver) the poller binds to that
	// ctx — but startIntake is restartable, so when this drive ends and a serve Receive
	// runs next, the poller is revived under the long-lived serve ctx. Ask thus can never
	// permanently wedge intake.
	b.startIntake(ctx)

	keyboard := map[string]any{"inline_keyboard": [][]map[string]any{{
		{"text": "✅ Yes", "callback_data": "yes:" + id},
		{"text": "❌ No", "callback_data": "no:" + id},
	}}}
	if err := b.call(ctx, "sendMessage", map[string]any{
		"chat_id": chatID, "text": "GATE — " + question, "reply_markup": keyboard,
	}, nil); err != nil {
		return false, err
	}
	select {
	case ans := <-reply:
		return ans, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// startIntake launches the SINGLE poller goroutine once. Exactly one goroutine ever
// advances the offset / calls getUpdates, so Receive and any number of concurrent Asks
// never double-deliver updates.
//
// The poller is bound to the STARTING caller's ctx, but startIntake is RESTARTABLE: the
// intake goroutine clears intakeStarted on exit (see intake), so if the poller's ctx is
// cancelled the NEXT startIntake spins a fresh poller. This closes the fragile coupling
// the review flagged: in the normal serve path Receive starts intake with the long-lived
// serve ctx, but a gate Ask (bound to a per-drive cancellable ctx via channel.Approver)
// can also be the first caller. Before, the sole poller latched to that per-drive ctx and
// died — permanently — when the drive ended. Now a later Receive simply restarts it, so a
// per-drive starter can never wedge intake.
func (b *Bot) startIntake(ctx context.Context) {
	b.smu.Lock()
	start := !b.intakeStarted
	if start {
		b.intakeStarted = true
	}
	b.smu.Unlock()
	if start {
		go b.intake(ctx)
	}
}

// intake is the sole poller: it long-polls getUpdates and routes each update — a gate
// answer to the waiting Ask, an ask_user tap or a message to the task queue. On exit
// (its ctx cancelled) it clears intakeStarted under smu so the NEXT startIntake spins a
// fresh poller — the property that makes a per-drive starter (a gate Ask) unable to wedge
// intake: when its ctx ends, the poller stops AND unlatches, and the next Receive/Ask
// restarts it under a live ctx. Exactly one poller still runs at a time (the flag gates
// starts; clearing it happens only after this goroutine has returned from its loop).
func (b *Bot) intake(ctx context.Context) {
	defer func() {
		b.smu.Lock()
		b.intakeStarted = false
		b.smu.Unlock()
	}()
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		ups, err := b.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(retryWait) // graceful: network blip, retry
			continue
		}
		for _, u := range ups {
			b.route(ctx, u)
		}
	}
}

// route classifies one update. A gate answer for a PENDING gate is delivered to that
// gate's waiter (after the authorizer check + the callback ack); an ask_user tap or a
// message becomes a task request. A stale/foreign callback is dropped.
func (b *Bot) route(ctx context.Context, u tgUpdate) {
	if u.CallbackQuery != nil {
		data := u.CallbackQuery.Data
		if g := b.lookupGate(gateID(data)); g != nil {
			clicker := strconv.FormatInt(u.CallbackQuery.From.ID, 10)
			if b.authorize != nil && !b.authorize(clicker) {
				if b.log != nil {
					b.log.Append(eventlog.Event{Kind: "unauthorized_gate",
						Detail: map[string]any{"principal": clicker, "thread": g.thread}})
				}
				_ = b.call(ctx, "answerCallbackQuery", map[string]any{
					"callback_query_id": u.CallbackQuery.ID, "text": "Not authorized to answer this gate."}, nil)
				return // ignore; the gate keeps waiting for an authorized responder
			}
			_ = b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": u.CallbackQuery.ID}, nil)
			select {
			case g.reply <- strings.HasPrefix(data, "yes:"):
			default: // already answered (buffered 1) — first answer wins
			}
			return
		}
		// Not a gate answer: an ask_user tap becomes an ORDINARY authorized task request
		// (the answer line, Sender=clicker), flowing through the same intake→Permit→Turn
		// path a typed message does (I7).
		if strings.HasPrefix(data, "ask:") {
			if tr := b.handleAskCallback(ctx, u.CallbackQuery); tr != nil {
				b.pushPending(*tr)
			}
		}
		return
	}
	if u.Message != nil && strings.TrimSpace(u.Message.Text) != "" {
		b.pushPending(toRequest(u.Message))
	}
}

// gateID extracts the gate id from a callback ("yes:ask-3" / "no:ask-3" → "ask-3").
func gateID(data string) string {
	if i := strings.IndexByte(data, ':'); i >= 0 {
		return data[i+1:]
	}
	return ""
}

func (b *Bot) registerGate(id, thread string, reply chan bool) {
	b.smu.Lock()
	b.gates[id] = &gateEntry{reply: reply, thread: thread}
	b.smu.Unlock()
}

func (b *Bot) unregisterGate(id string) {
	b.smu.Lock()
	delete(b.gates, id)
	b.smu.Unlock()
}

func (b *Bot) lookupGate(id string) *gateEntry {
	b.smu.Lock()
	defer b.smu.Unlock()
	return b.gates[id]
}

func toRequest(m *tgMessage) channel.TaskRequest {
	return channel.TaskRequest{
		Goal:     m.Text,
		Sender:   strconv.FormatInt(m.From.ID, 10),
		ThreadID: strconv.FormatInt(m.Chat.ID, 10),
	}
}

// poll fetches the next batch of updates (long poll) and advances the offset. The
// offset read/write is smu-guarded (Receive and a gate Ask loop can poll
// concurrently); the network call itself runs OUTSIDE the lock so a 50s long poll
// never serializes the other loop.
func (b *Bot) poll(ctx context.Context) ([]tgUpdate, error) {
	b.smu.Lock()
	off := b.offset
	b.smu.Unlock()

	var r updatesResp
	if err := b.call(ctx, "getUpdates", map[string]any{"offset": off + 1, "timeout": 50}, &r); err != nil {
		return nil, err
	}

	b.smu.Lock()
	for _, u := range r.Result {
		if u.UpdateID > b.offset {
			b.offset = u.UpdateID
		}
	}
	b.smu.Unlock()
	return r.Result, nil
}

func (b *Bot) call(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := b.baseURL + "/bot" + b.token + "/" + method
	// Bounded retry on HTTP 429 so a rate-limited gate/ask/final message is not silently
	// dropped: honor the retry_after Telegram returns (JSON parameters or Retry-After
	// header), cap the wait, and give up after maxRateRetries. The body bytes are reusable,
	// so each attempt rebuilds its own request. Both error paths redact the token: it rides
	// in the URL path, so a *url.Error would otherwise leak it into a log (I3).
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return b.redactErr(method, err)
		}
		req.Header.Set("content-type", "application/json")
		resp, err := b.http.Do(req)
		if err != nil {
			// The token is embedded in the URL, so a transport failure is a *url.Error whose
			// string contains it — scrub so the secret can never reach a log (I3).
			return scrubToken(err, b.token)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRateRetries {
			if err := sleepCtx(ctx, telegramRetryAfter(resp.Header.Get("Retry-After"), raw)); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("telegram %s: %s", method, resp.Status)
		}
		if out != nil {
			return json.Unmarshal(raw, out)
		}
		return nil
	}
}

// Rate-limit backoff knobs (see call). maxRateRetries bounds the retry count; the wait is
// floored at rateRetryDefault and capped at rateRetryMax so an absurd retry_after can never
// park a call for long. rateRetryDefault is a var only so tests can shrink it.
const (
	maxRateRetries = 3
	rateRetryMax   = 30 * time.Second
)

var rateRetryDefault = 1 * time.Second

// telegramRetryAfter derives a bounded backoff from a 429 response: Telegram carries the
// hint in the JSON body (parameters.retry_after) and sometimes a Retry-After header. A
// missing/garbage value falls back to rateRetryDefault; the result is capped at rateRetryMax.
func telegramRetryAfter(header string, body []byte) time.Duration {
	secs := 0
	var r struct {
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if json.Unmarshal(body, &r) == nil && r.Parameters.RetryAfter > 0 {
		secs = r.Parameters.RetryAfter
	} else if n, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && n > 0 {
		secs = n
	}
	d := rateRetryDefault
	if secs > 0 {
		d = time.Duration(secs) * time.Second
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

// scrubToken removes the bot token from an error's text. The token is embedded in every
// request URL, so a transport failure yields a *url.Error whose message contains it;
// callers discard these today, but scrubbing here guarantees the secret can never surface
// in a future log (I3 defense-in-depth). Errors without the token are returned unchanged,
// preserving their wrap chain.
func scrubToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	if s := err.Error(); strings.Contains(s, token) {
		return errors.New(strings.ReplaceAll(s, token, "<redacted>"))
	}
	return err
}

// redactErr strips the bot token from an error before returning it. Telegram puts the
// token in the request URL PATH, so a *url.Error from the HTTP layer carries it in
// Error() (Go redacts only URL userinfo, not the path). Wrapping here means a token
// can never ride a call error into a log or the append-only event trail (I3),
// regardless of what a future caller does with the error. Slack does this correctly
// by construction (token in the Authorization header, never the URL).
func (b *Bot) redactErr(method string, err error) error {
	msg := err.Error()
	if b.token != "" {
		msg = strings.ReplaceAll(msg, b.token, "[redacted]")
	}
	return fmt.Errorf("telegram %s: %s", method, msg)
}
