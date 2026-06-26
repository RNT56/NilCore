package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"nilcore/internal/channel"
)

// apiOK returns an httptest server whose every Web API call replies ok (chat.postMessage
// also returns a ts), recording chat.postMessage bodies so a test can assert the blocks.
func apiOK(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/chat.postMessage") {
			mu.Lock()
			posts = append(posts, string(body))
			mu.Unlock()
		}
		_, _ = io.WriteString(w, `{"ok":true,"ts":"111"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &posts
}

func ev(payload string) event {
	return event{Type: "interactive", EnvelopeID: "e", Payload: json.RawMessage(payload)}
}

// TestSlackPostChoicesRendersBlocks asserts a prompt posts a Block Kit actions block
// whose button values carry the token + index.
func TestSlackPostChoicesRendersBlocks(t *testing.T) {
	srv, posts := apiOK(t)
	b := New("a", "x")
	b.apiBase = srv.URL
	if err := b.PostChoices(context.Background(), "C9", "Which db?", []channel.AskChoice{{Label: "PG"}, {Label: "SQLite"}}, false); err != nil {
		t.Fatal(err)
	}
	if len(*posts) != 1 {
		t.Fatalf("want one postMessage, got %d", len(*posts))
	}
	for _, want := range []string{"actions", "PG", "SQLite", "ask:k1:0", "ask:k1:1", "Which db?"} {
		if !strings.Contains((*posts)[0], want) {
			t.Errorf("posted blocks missing %q", want)
		}
	}
}

// TestSlackAskClickResolves: a single-select click becomes a TaskRequest with the
// formatted line, the clicker as Sender, and the channel as ThreadID.
func TestSlackAskClickResolves(t *testing.T) {
	srv, _ := apiOK(t)
	b := New("a", "x")
	b.apiBase = srv.URL
	if err := b.PostChoices(context.Background(), "C9", "Q", []channel.AskChoice{{Label: "PG"}, {Label: "SQLite"}}, false); err != nil {
		t.Fatal(err)
	}
	b.src = &fakeSource{events: []event{ev(`{"type":"block_actions","user":{"id":"U42"},"channel":{"id":"C9"},"message":{"ts":"111"},"actions":[{"value":"ask:k1:1"}]}`)}}
	tr, err := b.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tr.Goal != "2" || tr.Sender != "U42" || tr.ThreadID != "C9" {
		t.Fatalf("click → %+v, want Goal=2 Sender=U42 Thread=C9", tr)
	}
}

// TestSlackAskUnauthorizedIgnored: an unauthorized clicker's tap is dropped; an
// authorized message in the same stream is what Receive returns.
func TestSlackAskUnauthorizedIgnored(t *testing.T) {
	srv, _ := apiOK(t)
	b := New("a", "x")
	b.apiBase = srv.URL
	b.SetAuthorizer(func(p string) bool { return p == "U42" }, nil)
	if err := b.PostChoices(context.Background(), "C9", "Q", []channel.AskChoice{{Label: "A"}, {Label: "B"}}, false); err != nil {
		t.Fatal(err)
	}
	b.src = &fakeSource{events: []event{
		ev(`{"type":"block_actions","user":{"id":"U666"},"channel":{"id":"C9"},"message":{"ts":"111"},"actions":[{"value":"ask:k1:0"}]}`),
		{Type: "events_api", EnvelopeID: "e2", Payload: json.RawMessage(`{"event":{"type":"message","text":"typed answer","user":"U42","channel":"C9"}}`)},
	}}
	tr, err := b.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tr.Goal != "typed answer" || tr.Sender != "U42" {
		t.Fatalf("unauthorized click must be dropped; got %+v", tr)
	}
}

// TestSlackMultiSelectDone: toggle clicks accumulate; Done finalizes into "i,j".
func TestSlackMultiSelectDone(t *testing.T) {
	srv, _ := apiOK(t)
	b := New("a", "x")
	b.apiBase = srv.URL
	if err := b.PostChoices(context.Background(), "C9", "pick", []channel.AskChoice{{Label: "A"}, {Label: "B"}, {Label: "C"}}, true); err != nil {
		t.Fatal(err)
	}
	b.src = &fakeSource{events: []event{
		ev(`{"type":"block_actions","user":{"id":"U42"},"channel":{"id":"C9"},"message":{"ts":"111"},"actions":[{"value":"ask:k1:t0"}]}`),
		ev(`{"type":"block_actions","user":{"id":"U42"},"channel":{"id":"C9"},"message":{"ts":"111"},"actions":[{"value":"ask:k1:t2"}]}`),
		ev(`{"type":"block_actions","user":{"id":"U42"},"channel":{"id":"C9"},"message":{"ts":"111"},"actions":[{"value":"ask:k1:done"}]}`),
	}}
	tr, err := b.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tr.Goal != "1,3" {
		t.Fatalf("multi-select Done → %q, want \"1,3\"", tr.Goal)
	}
}
