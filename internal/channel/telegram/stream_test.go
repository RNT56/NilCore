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

	if err := b.FinalizeRich(context.Background(), "99", "*done* `code`"); err != nil {
		t.Fatalf("FinalizeRich: %v", err)
	}
	if got["parse_mode"] != "MarkdownV2" || got["text"] != "*done* `code`" || got["chat_id"].(float64) != 99 {
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
