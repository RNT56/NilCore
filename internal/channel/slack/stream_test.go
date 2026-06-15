package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recorder captures the chat.postMessage / chat.update calls a streaming run makes.
type recorder struct {
	mu      sync.Mutex
	posts   []map[string]any
	updates []map[string]any
}

func (rec *recorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		defer rec.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat.postMessage"):
			rec.posts = append(rec.posts, body)
			_, _ = io.WriteString(w, fmt.Sprintf(`{"ok":true,"ts":"%d.0"}`, len(rec.posts)))
		case strings.HasSuffix(r.URL.Path, "/chat.update"):
			rec.updates = append(rec.updates, body)
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSlackStreamDraftEditsInPlace(t *testing.T) {
	rec := &recorder{}
	b := New("app", "bot")
	b.apiBase = rec.server(t).URL
	ctx := context.Background()

	// First token of draft 1 posts; a later token of the same draft edits in place.
	if err := b.StreamDraft(ctx, "C1", 1, "Hel"); err != nil {
		t.Fatal(err)
	}
	if err := b.StreamDraft(ctx, "C1", 1, "Hello"); err != nil {
		t.Fatal(err)
	}
	// A NEW draft id posts a fresh message rather than editing the prior one.
	if err := b.StreamDraft(ctx, "C1", 2, "New turn"); err != nil {
		t.Fatal(err)
	}
	// Finalize commits the active (draft-2) message in place and forgets it.
	if err := b.FinalizeRich(ctx, "C1", "done & dusted"); err != nil {
		t.Fatal(err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.posts) != 2 {
		t.Fatalf("posts = %d, want 2 (one per draft id)", len(rec.posts))
	}
	if rec.posts[0]["text"] != "Hel" || rec.posts[1]["text"] != "New turn" {
		t.Errorf("post texts = %q, %q", rec.posts[0]["text"], rec.posts[1]["text"])
	}
	// Two edits: the draft-1 in-place token, and the finalize of draft 2.
	if len(rec.updates) != 2 {
		t.Fatalf("updates = %d, want 2", len(rec.updates))
	}
	if rec.updates[0]["ts"] != "1.0" || rec.updates[0]["text"] != "Hello" {
		t.Errorf("first edit = ts %v text %q", rec.updates[0]["ts"], rec.updates[0]["text"])
	}
	if rec.updates[1]["ts"] != "2.0" || rec.updates[1]["text"] != "done &amp; dusted" {
		t.Errorf("finalize edit = ts %v text %q (want ts 2.0, escaped &)", rec.updates[1]["ts"], rec.updates[1]["text"])
	}
}

func TestEscapeSlack(t *testing.T) {
	if got := escapeSlack("a < b & c > d"); got != "a &lt; b &amp; c &gt; d" {
		t.Errorf("escapeSlack = %q", got)
	}
	if escapeSlack("plain text") != "plain text" {
		t.Error("text with no specials must be unchanged")
	}
}
