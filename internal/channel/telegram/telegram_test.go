package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nilcore/internal/channel"
)

func ctx5(t *testing.T) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func TestReceive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":1,"message":{"from":{"id":42},"chat":{"id":99},"text":"fix the bug"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	b := New("token")
	b.baseURL = srv.URL
	ctx, cancel := ctx5(t)
	defer cancel()

	tr, err := b.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if tr.Goal != "fix the bug" || tr.ThreadID != "99" || tr.Sender != "42" {
		t.Fatalf("got %+v", tr)
	}
}

func TestUpdate(t *testing.T) {
	var gotChat float64
	var gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			gotChat, _ = body["chat_id"].(float64)
			gotText, _ = body["text"].(string)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	b := New("t")
	b.baseURL = srv.URL
	ctx, cancel := ctx5(t)
	defer cancel()

	if err := b.Update(ctx, "99", "working..."); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if gotChat != 99 || gotText != "working..." {
		t.Errorf("chat=%v text=%q", gotChat, gotText)
	}
}

func TestAsk(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want bool
	}{
		{"yes", "yes:ask-1", true},
		{"no", "no:ask-1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sawKeyboard bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				switch {
				case strings.HasSuffix(r.URL.Path, "/sendMessage"):
					if _, ok := body["reply_markup"]; ok {
						sawKeyboard = true
					}
					_, _ = io.WriteString(w, `{"ok":true}`)
				case strings.HasSuffix(r.URL.Path, "/getUpdates"):
					_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":7,"callback_query":{"id":"cb1","data":"`+tc.data+`","message":{"chat":{"id":99}}}}]}`)
				default:
					_, _ = io.WriteString(w, `{"ok":true}`)
				}
			}))
			b := New("t")
			b.baseURL = srv.URL
			ctx, cancel := ctx5(t)
			defer cancel()

			ok, err := b.Ask(ctx, "99", "merge to main?")
			srv.Close()
			if err != nil {
				t.Fatalf("Ask: %v", err)
			}
			if ok != tc.want {
				t.Errorf("Ask = %v, want %v", ok, tc.want)
			}
			if !sawKeyboard {
				t.Error("gate question lacked an inline keyboard")
			}
		})
	}
}

// TestAskRejectsUnauthorizedClicker proves a gate button click from a principal
// outside the allowlist is ignored (logged, kept waiting), and an authorized
// click is honored — even when both arrive in the same update batch.
func TestAskRejectsUnauthorizedClicker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			// First an intruder clicks yes, then the authorized user clicks no.
			_, _ = io.WriteString(w, `{"ok":true,"result":[`+
				`{"update_id":10,"callback_query":{"id":"cb1","from":{"id":666},"data":"yes:ask-1","message":{"chat":{"id":99}}}},`+
				`{"update_id":11,"callback_query":{"id":"cb2","from":{"id":42},"data":"no:ask-1","message":{"chat":{"id":99}}}}`+
				`]}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()

	b := New("t")
	b.baseURL = srv.URL
	b.SetAuthorizer(func(p string) bool { return p == "42" }, nil)

	ctx, cancel := ctx5(t)
	defer cancel()
	ok, err := b.Ask(ctx, "99", "merge to main?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	// The intruder's "yes" must NOT win; the authorized "no" decides.
	if ok {
		t.Fatal("an unauthorized clicker's approval was honored")
	}
}

// TestTelegramConcurrentReceiveAndAsk proves the single-poller demux: Receive and a gate
// Ask run CONCURRENTLY on one Bot; the gate callback routes to Ask and the message routes
// to Receive, with no double-delivery and `go test -race` clean (previously two
// concurrent poll() calls read the same offset and delivered every update twice).
func TestTelegramConcurrentReceiveAndAsk(t *testing.T) {
	var deliver atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			if deliver.CompareAndSwap(true, false) {
				_, _ = io.WriteString(w, `{"ok":true,"result":[`+
					`{"update_id":1,"callback_query":{"id":"cq1","from":{"id":42},"data":"yes:ask-1","message":{"chat":{"id":99}}}},`+
					`{"update_id":2,"message":{"from":{"id":42},"chat":{"id":99},"text":"do it"}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`) // sendMessage / answerCallbackQuery
	}))
	defer srv.Close()
	b := New("t")
	b.baseURL = srv.URL
	ctx, cancel := ctx5(t)
	defer cancel()

	askDone := make(chan bool, 1)
	go func() { ok, _ := b.Ask(ctx, "99", "merge?"); askDone <- ok }()
	waitGate(t, b, "ask-1") // Ask has registered its gate

	recvDone := make(chan channel.TaskRequest, 1)
	go func() { tr, _ := b.Receive(ctx); recvDone <- tr }()
	deliver.Store(true) // the next getUpdates returns the batch

	select {
	case ok := <-askDone:
		if !ok {
			t.Error("gate callback yes:ask-1 should resolve the gate to true")
		}
	case <-ctx.Done():
		t.Fatal("Ask never resolved")
	}
	select {
	case tr := <-recvDone:
		if tr.Goal != "do it" {
			t.Errorf("Receive got %+v, want goal 'do it'", tr)
		}
	case <-ctx.Done():
		t.Fatal("Receive never delivered the message")
	}
}

// TestIntakeRestartsAfterStarterCtxCancelled proves the review fix: if a gate Ask is the
// FIRST caller to start the poller and its (per-drive) ctx is then cancelled, the poller
// stops AND unlatches, so a subsequent Receive on a fresh, long-lived ctx revives it and
// still delivers messages. Before the fix the sole poller latched to the drive ctx and
// never restarted, wedging all future intake.
func TestIntakeRestartsAfterStarterCtxCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":5,"message":{"from":{"id":42},"chat":{"id":99},"text":"do it"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	b := New("t")
	b.baseURL = srv.URL

	// A gate Ask starts the poller under a per-drive ctx, then that ctx is cancelled.
	driveCtx, driveCancel := context.WithCancel(context.Background())
	b.startIntake(driveCtx) // stand in for Ask's startIntake with the drive ctx
	driveCancel()

	// Wait for the poller to observe cancellation and unlatch (intakeStarted → false).
	deadline := time.Now().Add(3 * time.Second)
	for {
		b.smu.Lock()
		started := b.intakeStarted
		b.smu.Unlock()
		if !started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poller never unlatched after its starting ctx was cancelled")
		}
		time.Sleep(time.Millisecond)
	}

	// A fresh Receive on a live ctx must revive the poller and deliver the message.
	ctx, cancel := ctx5(t)
	defer cancel()
	tr, err := b.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive after restart: %v", err)
	}
	if tr.Goal != "do it" {
		t.Fatalf("got %+v, want goal 'do it' after poller restart", tr)
	}
}

func waitGate(t *testing.T, b *Bot, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b.lookupGate(id) != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("gate %q never registered", id)
}

// errRT is a RoundTripper that always fails, so http.Client.Do wraps its error in a
// *url.Error carrying the request URL (which embeds the bot token in its path).
type errRT struct{ err error }

func (e errRT) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }

// TestCallScrubsTokenFromTransportError proves a transport failure never leaks the bot
// token: the token is in the request URL, so the *url.Error string would otherwise contain
// it. call must redact it (I3 defense-in-depth) even though callers discard the error today.
func TestCallScrubsTokenFromTransportError(t *testing.T) {
	const token = "123456:SUPERSECRETTOKEN"
	b := New(token)
	b.baseURL = "http://telegram.invalid"
	b.http = &http.Client{Transport: errRT{err: errors.New("dial tcp: connection refused")}}

	err := b.call(context.Background(), "getUpdates", map[string]any{"x": 1}, nil)
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "SUPERSECRET") {
		t.Fatalf("bot token leaked in error: %q", err.Error())
	}
}

// TestUpdateClipsLongMessage proves a progress line over the Bot API cap is clipped, so it
// is not sent whole (→ HTTP 400 → the whole update dropped).
func TestUpdateClipsLongMessage(t *testing.T) {
	var gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotText, _ = body["text"].(string)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	b := New("t")
	b.baseURL = srv.URL
	ctx, cancel := ctx5(t)
	defer cancel()

	if err := b.Update(ctx, "99", strings.Repeat("a", 5000)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if n := len([]rune(gotText)); n > telegramTextLimit {
		t.Fatalf("Update text not clipped: %d runes (cap %d)", n, telegramTextLimit)
	}
}

// TestTelegramRetryAfterParsing pins the bounded backoff and that the JSON body's
// parameters.retry_after wins over the header.
func TestTelegramRetryAfterParsing(t *testing.T) {
	if got := telegramRetryAfter("", []byte(`{"parameters":{"retry_after":4}}`)); got != 4*time.Second {
		t.Errorf("body retry_after=4 → %v, want 4s", got)
	}
	if got := telegramRetryAfter("2", []byte(`{}`)); got != 2*time.Second {
		t.Errorf("header Retry-After=2 → %v, want 2s", got)
	}
	if got := telegramRetryAfter("", []byte(`{}`)); got != rateRetryDefault {
		t.Errorf("no hint → %v, want default %v", got, rateRetryDefault)
	}
	if got := telegramRetryAfter("", []byte(`{"parameters":{"retry_after":99999}}`)); got != rateRetryMax {
		t.Errorf("absurd retry_after → %v, want cap %v", got, rateRetryMax)
	}
}

// TestTelegramCallRetriesOn429 proves a rate-limited call is retried (bounded) then
// succeeds — the gate/ask/final message is not dropped to a 429.
func TestTelegramCallRetriesOn429(t *testing.T) {
	old := rateRetryDefault
	rateRetryDefault = time.Millisecond
	defer func() { rateRetryDefault = old }()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{}`) // no retry_after → default (1ms in test)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	b := New("t")
	b.baseURL = srv.URL
	if err := b.Update(context.Background(), "99", "hi"); err != nil {
		t.Fatalf("Update after a 429 retry: %v", err)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("server hit %d times, want 2", n)
	}
}
