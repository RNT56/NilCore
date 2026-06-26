// Package telegram drives NilCore from a phone over the Telegram Bot API: text
// messages become task requests, progress streams back as messages, and an
// irreversible-action gate renders as inline Yes/No buttons whose answer feeds
// policy.Approver (via channel.NewApprover). Stdlib HTTP only (invariant I6).
package telegram

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

const defaultAPIBase = "https://api.telegram.org"

// retryWait is how long to pause after a transient poll error before retrying.
var retryWait = 2 * time.Second

// Bot is a long-polling Telegram channel. It implements channel.Channel.
type Bot struct {
	token   string
	baseURL string
	http    *http.Client

	offset  int                   // last seen update_id
	askSeq  atomic.Int64          // unique gate/ask-callback ids
	pending []channel.TaskRequest // task messages (and resolved ask taps) seen while awaiting a gate answer

	amu  sync.Mutex           // guards asks
	asks map[string]*askEntry // ask_user choice prompts awaiting a tap, by correlation token

	authorize func(string) bool // who may answer a gate / ask (nil = anyone; serve sets it)
	log       *eventlog.Log     // for recording rejected gate/ask clicks (may be nil)
}

var _ channel.Channel = (*Bot)(nil)

// New returns a bot for the given TELEGRAM_BOT_TOKEN.
func New(token string) *Bot {
	return &Bot{
		token:   token,
		baseURL: defaultAPIBase,
		asks:    map[string]*askEntry{},
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

// Receive blocks until the next text message, returning it as a task request.
func (b *Bot) Receive(ctx context.Context) (channel.TaskRequest, error) {
	for {
		if len(b.pending) > 0 {
			tr := b.pending[0]
			b.pending = b.pending[1:]
			return tr, nil
		}
		if err := ctx.Err(); err != nil {
			return channel.TaskRequest{}, err
		}
		ups, err := b.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return channel.TaskRequest{}, ctx.Err()
			}
			time.Sleep(retryWait) // graceful: network blip, retry
			continue
		}
		var got *channel.TaskRequest
		for _, u := range ups {
			// An ask_user button tap (callback) becomes an ORDINARY authorized task
			// request carrying the formatted answer line — it then flows through the
			// same intake→Permit→Turn→Resolve path a typed message does (I7: no new
			// trust promotion). A toggle/unauthorized/stale tap yields nil.
			if u.CallbackQuery != nil {
				if tr := b.handleAskCallback(ctx, u.CallbackQuery); tr != nil {
					if got == nil {
						got = tr
					} else {
						b.pending = append(b.pending, *tr)
					}
				}
				continue
			}
			if u.Message == nil || strings.TrimSpace(u.Message.Text) == "" {
				continue
			}
			tr := toRequest(u.Message)
			if got == nil {
				got = &tr
			} else {
				b.pending = append(b.pending, tr) // don't drop concurrent messages
			}
		}
		if got != nil {
			return *got, nil
		}
	}
}

// Update sends a progress line to the thread (chat).
func (b *Bot) Update(ctx context.Context, threadID, message string) error {
	chatID, err := strconv.ParseInt(threadID, 10, 64)
	if err != nil {
		return fmt.Errorf("bad thread id %q: %w", threadID, err)
	}
	return b.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": message}, nil)
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
	keyboard := map[string]any{"inline_keyboard": [][]map[string]any{{
		{"text": "✅ Yes", "callback_data": "yes:" + id},
		{"text": "❌ No", "callback_data": "no:" + id},
	}}}
	if err := b.call(ctx, "sendMessage", map[string]any{
		"chat_id": chatID, "text": "GATE — " + question, "reply_markup": keyboard,
	}, nil); err != nil {
		return false, err
	}

	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		ups, err := b.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			time.Sleep(retryWait)
			continue
		}
		for _, u := range ups {
			if u.CallbackQuery != nil && strings.HasSuffix(u.CallbackQuery.Data, ":"+id) {
				clicker := strconv.FormatInt(u.CallbackQuery.From.ID, 10)
				if b.authorize != nil && !b.authorize(clicker) {
					if b.log != nil {
						b.log.Append(eventlog.Event{Kind: "unauthorized_gate",
							Detail: map[string]any{"principal": clicker, "thread": threadID}})
					}
					_ = b.call(ctx, "answerCallbackQuery", map[string]any{
						"callback_query_id": u.CallbackQuery.ID, "text": "Not authorized to answer this gate."}, nil)
					continue // ignore; keep waiting for an authorized responder
				}
				_ = b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": u.CallbackQuery.ID}, nil)
				return strings.HasPrefix(u.CallbackQuery.Data, "yes:"), nil
			}
			// An ask_user button tap that lands in THIS gate's poll loop (the gate and
			// Receive can race the shared offset) must not be dropped: resolve it and
			// buffer the resulting answer task so Receive delivers it. A toggle/stale/
			// unauthorized tap yields nil.
			if u.CallbackQuery != nil && strings.HasPrefix(u.CallbackQuery.Data, "ask:") {
				if tr := b.handleAskCallback(ctx, u.CallbackQuery); tr != nil {
					b.pending = append(b.pending, *tr)
				}
				continue
			}
			if u.Message != nil && strings.TrimSpace(u.Message.Text) != "" {
				b.pending = append(b.pending, toRequest(u.Message)) // buffer, don't drop
			}
		}
	}
}

func toRequest(m *tgMessage) channel.TaskRequest {
	return channel.TaskRequest{
		Goal:     m.Text,
		Sender:   strconv.FormatInt(m.From.ID, 10),
		ThreadID: strconv.FormatInt(m.Chat.ID, 10),
	}
}

// poll fetches the next batch of updates (long poll) and advances the offset.
func (b *Bot) poll(ctx context.Context) ([]tgUpdate, error) {
	var r updatesResp
	if err := b.call(ctx, "getUpdates", map[string]any{"offset": b.offset + 1, "timeout": 50}, &r); err != nil {
		return nil, err
	}
	for _, u := range r.Result {
		if u.UpdateID > b.offset {
			b.offset = u.UpdateID
		}
	}
	return r.Result, nil
}

func (b *Bot) call(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := b.baseURL + "/bot" + b.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
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
		return fmt.Errorf("telegram %s: %s", method, resp.Status)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}
