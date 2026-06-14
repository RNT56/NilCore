package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
