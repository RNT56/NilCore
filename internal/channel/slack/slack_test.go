package slack

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeSource struct {
	events []event
	i      int
	acked  []string
}

func (f *fakeSource) Next(ctx context.Context) (event, error) {
	if f.i >= len(f.events) {
		<-ctx.Done()
		return event{}, ctx.Err()
	}
	e := f.events[f.i]
	f.i++
	return e, nil
}
func (f *fakeSource) Ack(_ context.Context, id string) error {
	f.acked = append(f.acked, id)
	return nil
}
func (f *fakeSource) Close() error { return nil }

func TestReceive(t *testing.T) {
	b := New("app", "bot")
	b.src = &fakeSource{events: []event{{
		Type:       "events_api",
		EnvelopeID: "e1",
		Payload:    json.RawMessage(`{"event":{"type":"message","text":"fix it","user":"U1","channel":"C9"}}`),
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tr, err := b.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if tr.Goal != "fix it" || tr.ThreadID != "C9" || tr.Sender != "U1" {
		t.Fatalf("got %+v", tr)
	}
}

func TestReceiveIgnoresBot(t *testing.T) {
	b := New("app", "bot")
	src := &fakeSource{events: []event{
		{Type: "events_api", EnvelopeID: "e1", Payload: json.RawMessage(`{"event":{"type":"message","text":"echo","bot_id":"B1","channel":"C9"}}`)},
		{Type: "events_api", EnvelopeID: "e2", Payload: json.RawMessage(`{"event":{"type":"message","text":"real","user":"U1","channel":"C9"}}`)},
	}}
	b.src = src
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tr, err := b.Receive(ctx)
	if err != nil || tr.Goal != "real" {
		t.Fatalf("expected to skip bot message; got %+v, %v", tr, err)
	}
}

func TestUpdate(t *testing.T) {
	var gotChan, gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotChan, _ = body["channel"].(string)
		gotText, _ = body["text"].(string)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	b := New("app", "bot")
	b.apiBase = srv.URL
	if err := b.Update(context.Background(), "C9", "working..."); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if gotChan != "C9" || gotText != "working..." {
		t.Errorf("chan=%q text=%q", gotChan, gotText)
	}
}

func TestAsk(t *testing.T) {
	var sawBlocks bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["blocks"]; ok {
			sawBlocks = true
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	b := New("app", "bot")
	b.apiBase = srv.URL
	b.src = &fakeSource{events: []event{{
		Type:       "interactive",
		EnvelopeID: "e1",
		Payload:    json.RawMessage(`{"type":"block_actions","actions":[{"value":"yes:ask-1"}],"channel":{"id":"C9"}}`),
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ok, err := b.Ask(ctx, "C9", "merge to main?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !ok {
		t.Error("expected yes")
	}
	if !sawBlocks {
		t.Error("gate question lacked Block Kit buttons")
	}
}

// TestWSFrame round-trips WriteText → readFrame across a pipe (client frames are
// masked; the reader unmasks them).
func TestWSFrame(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	client := &wsConn{conn: c1, r: bufio.NewReader(c1)}
	server := &wsConn{conn: c2, r: bufio.NewReader(c2)}

	go func() { _ = client.WriteText("hello ws") }()

	op, payload, err := server.readFrame()
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if op != 0x1 || string(payload) != "hello ws" {
		t.Fatalf("frame op=%x payload=%q", op, payload)
	}
}

// TestAskRejectsUnauthorizedClicker proves a Block Kit gate click from a user
// outside the allowlist is ignored, and the authorized user's answer decides.
func TestAskRejectsUnauthorizedClicker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	b := New("app", "bot")
	b.apiBase = srv.URL
	b.SetAuthorizer(func(u string) bool { return u == "U1" }, nil)
	b.src = &fakeSource{events: []event{
		{Type: "interactive", EnvelopeID: "e1", Payload: json.RawMessage(
			`{"type":"block_actions","user":{"id":"UHACK"},"actions":[{"value":"yes:ask-1"}],"channel":{"id":"C9"}}`)},
		{Type: "interactive", EnvelopeID: "e2", Payload: json.RawMessage(
			`{"type":"block_actions","user":{"id":"U1"},"actions":[{"value":"no:ask-1"}],"channel":{"id":"C9"}}`)},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ok, err := b.Ask(ctx, "C9", "merge to main?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if ok {
		t.Fatal("an unauthorized clicker's approval was honored")
	}
}
