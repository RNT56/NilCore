package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureBot returns a Bot pointed at a fake API server that records the JSON body
// of the request whose method path ends in want.
func captureBot(t *testing.T, want string, got *map[string]any) *Bot {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/"+want) {
			_ = json.NewDecoder(r.Body).Decode(got)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	b := New("tok")
	b.baseURL = srv.URL
	return b
}

func TestStreamDraft(t *testing.T) {
	var got map[string]any
	b := captureBot(t, "sendMessageDraft", &got)

	if err := b.StreamDraft(context.Background(), "99", 7, "Hello wor"); err != nil {
		t.Fatalf("StreamDraft: %v", err)
	}
	if got["chat_id"].(float64) != 99 || got["draft_id"].(float64) != 7 || got["text"].(string) != "Hello wor" {
		t.Errorf("sendMessageDraft payload = %v", got)
	}

	// The API requires a non-zero draft_id; a 0 must be coerced.
	got = nil
	if err := b.StreamDraft(context.Background(), "99", 0, "x"); err != nil {
		t.Fatal(err)
	}
	if got["draft_id"].(float64) == 0 {
		t.Error("draft_id 0 must be coerced to a non-zero value")
	}

	// A bad thread id is a clear error, not a malformed call.
	if err := b.StreamDraft(context.Background(), "not-an-int", 1, "x"); err == nil {
		t.Error("expected an error for a non-numeric thread id")
	}
}

func TestFinalizeRich(t *testing.T) {
	var got map[string]any
	b := captureBot(t, "sendMessage", &got)

	// FinalizeRich takes PLAIN text and escapes it for MarkdownV2, so arbitrary
	// prose (with reserved chars) renders safely.
	if err := b.FinalizeRich(context.Background(), "99", "done: 2+2!"); err != nil {
		t.Fatalf("FinalizeRich: %v", err)
	}
	if got["parse_mode"] != "MarkdownV2" || got["text"] != `done: 2\+2\!` || got["chat_id"].(float64) != 99 {
		t.Errorf("finalize payload = %v", got)
	}
}

func TestEscapeMarkdownV2(t *testing.T) {
	if got := EscapeMarkdownV2("a_b*c.d!"); got != `a\_b\*c\.d\!` {
		t.Errorf("escape = %q", got)
	}
	if EscapeMarkdownV2("plain text with spaces") != "plain text with spaces" {
		t.Error("text with no specials must be unchanged")
	}
	// A literal backslash (a Windows path, a regex) must itself be escaped — '\' is
	// MarkdownV2's escape char, so an unescaped one breaks the next reserved char.
	if got := EscapeMarkdownV2(`C:\d+`); got != `C:\\d\+` {
		t.Errorf("backslash escape = %q, want %q", got, `C:\\d\+`)
	}
}

func TestClipText(t *testing.T) {
	long := strings.Repeat("a", 5000)
	if got := clipText(long); len([]rune(got)) != telegramTextLimit {
		t.Errorf("clipped len = %d, want %d", len([]rune(got)), telegramTextLimit)
	}
	if clipText("short") != "short" {
		t.Error("short text must be unchanged")
	}
}

// TestClipEscapeMarkdownV2 proves the rich finalize never produces a dangling
// backslash: a long, reserved-char-dense payload (whose escaped form exceeds the
// cap) is clipped on a whole escape pair, stays within the limit, and ends in a
// valid (non-reserved) ellipsis — so Telegram accepts it instead of 400-rejecting
// and losing the finalized message.
func TestClipEscapeMarkdownV2(t *testing.T) {
	// 5000 dots: each escapes to "\." so the escaped form is ~10000 runes, well over
	// the 4096 cap — exactly the boundary the naive escape-then-clip got wrong.
	got := clipEscapeMarkdownV2(strings.Repeat(".", 5000))
	if n := len([]rune(got)); n > telegramTextLimit {
		t.Fatalf("clipped escaped len = %d, want <= %d", n, telegramTextLimit)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clipped output must end in the ellipsis, got tail %q", lastRunes(got, 4))
	}
	// No dangling backslash: every '\' must be followed by the char it escapes, so
	// the count of backslashes equals the count of escape pairs (one '\.' each).
	if bs := strings.Count(got, "\\"); bs != strings.Count(got, `\.`) {
		t.Errorf("dangling backslash: %d backslashes vs %d escape pairs", bs, strings.Count(got, `\.`))
	}
	// Short reserved text is escaped fully, unclipped (no ellipsis).
	if g := clipEscapeMarkdownV2("a.b!"); g != `a\.b\!` {
		t.Errorf("short escape = %q, want %q", g, `a\.b\!`)
	}
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
