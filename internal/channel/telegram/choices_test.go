package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"nilcore/internal/channel"
)

// askServer mocks the Bot API for the ask-button flow: sendMessage returns a message id,
// getUpdates returns the scripted batches in order then empties, and the side-effect
// calls (answerCallbackQuery / editMessageReplyMarkup) return ok. It records sendMessage
// bodies so a test can assert the inline_keyboard.
func askServer(t *testing.T, batches ...string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			mu.Lock()
			sent = append(sent, string(body))
			mu.Unlock()
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":555}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			mu.Lock()
			b := `{"ok":true,"result":[]}`
			if i < len(batches) {
				b = batches[i]
				i++
			}
			mu.Unlock()
			_, _ = io.WriteString(w, b)
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &sent
}

// TestPostChoicesRendersButtons asserts a single-select prompt posts inline buttons whose
// callback_data carries the correlation token + choice index.
func TestPostChoicesRendersButtons(t *testing.T) {
	srv, sent := askServer(t)
	b := New("tok")
	b.baseURL = srv.URL
	if err := b.PostChoices(context.Background(), "99", "Which db?", []channel.AskChoice{{Label: "PG"}, {Label: "SQLite"}}, false); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 {
		t.Fatalf("want one sendMessage, got %d", len(*sent))
	}
	msg := (*sent)[0]
	for _, want := range []string{"inline_keyboard", "PG", "SQLite", "ask:k1:0", "ask:k1:1", "Which db?"} {
		if !strings.Contains(msg, want) {
			t.Errorf("posted message missing %q in:\n%s", want, msg)
		}
	}
}

// TestAskTapResolvesToAuthorizedTask: a single-select tap becomes a TaskRequest carrying
// the formatted answer line, the clicker as Sender, and the chat as ThreadID — the
// authorized path the rest of the answer routing relies on.
func TestAskTapResolvesToAuthorizedTask(t *testing.T) {
	srv, _ := askServer(t,
		`{"ok":true,"result":[{"update_id":1,"callback_query":{"id":"c1","from":{"id":42},"data":"ask:k1:1","message":{"message_id":555,"chat":{"id":99}}}}]}`)
	b := New("tok")
	b.baseURL = srv.URL
	if err := b.PostChoices(context.Background(), "99", "Which db?", []channel.AskChoice{{Label: "PG"}, {Label: "SQLite"}}, false); err != nil {
		t.Fatal(err)
	}
	tr, err := b.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tr.Goal != "2" || tr.Sender != "42" || tr.ThreadID != "99" {
		t.Fatalf("tap → %+v, want Goal=2 Sender=42 Thread=99", tr)
	}
}

// TestAskTapUnauthorizedIgnored: an unauthorized clicker's tap is dropped (logged), so a
// bystander cannot answer; an authorized message in the same batch is what Receive returns.
func TestAskTapUnauthorizedIgnored(t *testing.T) {
	srv, _ := askServer(t,
		`{"ok":true,"result":[`+
			`{"update_id":1,"callback_query":{"id":"c1","from":{"id":666},"data":"ask:k1:0","message":{"message_id":555,"chat":{"id":99}}}},`+
			`{"update_id":2,"message":{"from":{"id":42},"chat":{"id":99},"text":"typed answer"}}`+
			`]}`)
	b := New("tok")
	b.baseURL = srv.URL
	b.SetAuthorizer(func(p string) bool { return p == "42" }, nil)
	if err := b.PostChoices(context.Background(), "99", "Q", []channel.AskChoice{{Label: "A"}, {Label: "B"}}, false); err != nil {
		t.Fatal(err)
	}
	tr, err := b.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tr.Goal != "typed answer" || tr.Sender != "42" {
		t.Fatalf("unauthorized tap must be dropped; Receive returned %+v, want the typed answer", tr)
	}
}

// TestAskMultiSelectDone: toggle taps accumulate; Done finalizes the picks into the
// "i,j" line grammar.
func TestAskMultiSelectDone(t *testing.T) {
	srv, _ := askServer(t,
		`{"ok":true,"result":[`+
			`{"update_id":1,"callback_query":{"id":"c1","from":{"id":42},"data":"ask:k1:t0","message":{"message_id":555,"chat":{"id":99}}}},`+
			`{"update_id":2,"callback_query":{"id":"c2","from":{"id":42},"data":"ask:k1:t2","message":{"message_id":555,"chat":{"id":99}}}},`+
			`{"update_id":3,"callback_query":{"id":"c3","from":{"id":42},"data":"ask:k1:done","message":{"message_id":555,"chat":{"id":99}}}}`+
			`]}`)
	b := New("tok")
	b.baseURL = srv.URL
	if err := b.PostChoices(context.Background(), "99", "pick", []channel.AskChoice{{Label: "A"}, {Label: "B"}, {Label: "C"}}, true); err != nil {
		t.Fatal(err)
	}
	tr, err := b.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tr.Goal != "1,3" {
		t.Fatalf("multi-select Done → %q, want \"1,3\"", tr.Goal)
	}
}
