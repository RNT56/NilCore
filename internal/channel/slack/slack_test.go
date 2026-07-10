package slack

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nilcore/internal/channel"
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

// chanSource is a controllable eventSource for concurrency tests: the test pushes
// envelopes on ch and the single intake reads them in order.
type chanSource struct{ ch chan event }

func (c *chanSource) Next(ctx context.Context) (event, error) {
	select {
	case e := <-c.ch:
		return e, nil
	case <-ctx.Done():
		return event{}, ctx.Err()
	}
}
func (c *chanSource) Ack(context.Context, string) error { return nil }
func (c *chanSource) Close() error                      { return nil }

// TestSlackConcurrentReceiveAndAsk proves the single-intake demux: Receive and a gate
// Ask run CONCURRENTLY on one Bot, the gate answer routes to Ask and the message routes
// to Receive, and `go test -race` sees no data race over the socket (the bug this fix
// closes — previously both read b.src concurrently).
func TestSlackConcurrentReceiveAndAsk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"ts":"1"}`)
	}))
	defer srv.Close()
	b := New("app", "bot")
	b.apiBase = srv.URL
	src := &chanSource{ch: make(chan event, 8)}
	b.src = src
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	askDone := make(chan bool, 1)
	go func() { ok, _ := b.Ask(ctx, "C9", "merge?"); askDone <- ok }()
	waitGate(t, b, "ask-1") // Ask has registered its gate

	recvDone := make(chan channel.TaskRequest, 1)
	go func() { tr, _ := b.Receive(ctx); recvDone <- tr }()

	src.ch <- event{Type: "interactive", EnvelopeID: "e1", Payload: json.RawMessage(`{"type":"block_actions","actions":[{"value":"yes:ask-1"}],"channel":{"id":"C9"}}`)}
	src.ch <- event{Type: "events_api", EnvelopeID: "e2", Payload: json.RawMessage(`{"event":{"type":"message","text":"do it","user":"U1","channel":"C9"}}`)}

	select {
	case ok := <-askDone:
		if !ok {
			t.Error("gate answer yes:ask-1 should resolve the gate to true")
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

// TestSlackUpdateEscapesHostileText proves a serve progress line carrying untrusted
// repo/tool content is neutralized before Slack sees it: raw <!channel> (mass ping),
// <@U1> (mention), and <http://evil|ok> (forged link) must never reach a text field
// unescaped (I7). All serve surface lines flow through Update, so this is the main path.
func TestSlackUpdateEscapesHostileText(t *testing.T) {
	var gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotText, _ = body["text"].(string)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	b := New("app", "bot")
	b.apiBase = srv.URL
	if err := b.Update(context.Background(), "C9", "tool said: <!channel> see <http://evil|ok> & <@U1>"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	for _, hostile := range []string{"<!channel>", "<http://evil|ok>", "<@U1>"} {
		if strings.Contains(gotText, hostile) {
			t.Fatalf("hostile Slack markup reached the wire unescaped (%q) in %q", hostile, gotText)
		}
	}
	for _, want := range []string{"&lt;!channel&gt;", "&lt;http://evil|ok&gt;", "&lt;@U1&gt;", "&amp;"} {
		if !strings.Contains(gotText, want) {
			t.Errorf("escaped text missing %q in %q", want, gotText)
		}
	}
}

// TestSlackAskEscapesHostileQuestion proves the gate question is escaped in BOTH the
// Block Kit mrkdwn section and the notification fallback, so a model-authored gate that
// folds untrusted content cannot ping the channel or forge a link. (The values are read
// back DECODED — Slack's own JSON encodes '&' as &, so a raw-byte match would be
// checking the transport encoding, not the escape.)
func TestSlackAskEscapesHostileQuestion(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "blocks") {
			raw = b
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	b := New("app", "bot")
	b.apiBase = srv.URL
	b.src = &fakeSource{events: []event{{
		Type: "interactive", EnvelopeID: "e1",
		Payload: json.RawMessage(`{"type":"block_actions","actions":[{"value":"yes:ask-1"}],"channel":{"id":"C9"}}`),
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := b.Ask(ctx, "C9", "merge <!channel> now?"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	var body struct {
		Text   string `json:"text"`
		Blocks []struct {
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || len(body.Blocks) == 0 {
		t.Fatalf("decode gate body: %v (%s)", err, raw)
	}
	for name, got := range map[string]string{"section": body.Blocks[0].Text.Text, "fallback": body.Text} {
		if strings.Contains(got, "<!channel>") {
			t.Fatalf("gate %s carried an active <!channel>: %q", name, got)
		}
		if !strings.Contains(got, "&lt;!channel&gt;") {
			t.Errorf("gate %s was not escaped: %q", name, got)
		}
	}
}

// TestSlackAskClipsLargeEvidenceSection proves an evidence-rich gate (whose question
// exceeds Slack's 3000-char Block Kit section cap) still RENDERS and is answerable rather
// than erroring into a silent default-deny. The fake API rejects an oversized section
// exactly as real Slack does (invalid_blocks); the fix clips the section to fit.
func TestSlackAskClipsLargeEvidenceSection(t *testing.T) {
	var oversized bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Blocks []struct {
				Text struct {
					Text string `json:"text"`
				} `json:"text"`
			} `json:"blocks"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		for _, bl := range body.Blocks {
			if len([]rune(bl.Text.Text)) > slackSectionLimit {
				oversized = true
				_, _ = io.WriteString(w, `{"ok":false,"error":"invalid_blocks"}`)
				return
			}
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	b := New("app", "bot")
	b.apiBase = srv.URL
	b.src = &fakeSource{events: []event{{
		Type: "interactive", EnvelopeID: "e1",
		Payload: json.RawMessage(`{"type":"block_actions","actions":[{"value":"yes:ask-1"}],"channel":{"id":"C9"}}`),
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// ~6500 chars of "evidence" — well past the 3000 section cap.
	huge := "Approve this irreversible action?\n\n" + strings.Repeat("evidence line\n", 500)
	ok, err := b.Ask(ctx, "C9", huge)
	if err != nil {
		t.Fatalf("large-evidence gate errored (would default-deny unseen): %v", err)
	}
	if !ok {
		t.Fatal("gate should resolve yes")
	}
	if oversized {
		t.Fatal("section exceeded Slack's 3000-char cap — invalid_blocks would auto-deny the gate")
	}
}

// TestSlackPostChoicesEscapesHostileText proves an ask_user prompt escapes the (model-
// authored) question in its mrkdwn section and fallback text.
func TestSlackPostChoicesEscapesHostileText(t *testing.T) {
	srv, posts := apiOK(t)
	b := New("app", "bot")
	b.apiBase = srv.URL
	if err := b.PostChoices(context.Background(), "C9", "pick <!here> option", []channel.AskChoice{{Label: "A"}}, false); err != nil {
		t.Fatal(err)
	}
	if len(*posts) != 1 {
		t.Fatalf("want one postMessage, got %d", len(*posts))
	}
	if strings.Contains((*posts)[0], "<!here>") {
		t.Fatalf("ask_user question reached Slack with an active <!here>: %s", (*posts)[0])
	}
	var body struct {
		Text   string `json:"text"`
		Blocks []struct {
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal([]byte((*posts)[0]), &body); err != nil || len(body.Blocks) == 0 {
		t.Fatalf("decode PostChoices body: %v (%s)", err, (*posts)[0])
	}
	for name, got := range map[string]string{"section": body.Blocks[0].Text.Text, "fallback": body.Text} {
		if !strings.Contains(got, "&lt;!here&gt;") {
			t.Errorf("ask_user %s was not escaped: %q", name, got)
		}
	}
}

// TestSlackIntakeRestartsAfterStarterCtxCancelled proves the socket owner is restartable
// (mirrors telegram): if a gate Ask were the first startIntake caller and its per-drive
// ctx is cancelled, intake unlatches (intakeStarted → false) so a later Receive on a fresh
// ctx revives it and still delivers messages — instead of wedging Receive forever.
func TestSlackIntakeRestartsAfterStarterCtxCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"ts":"1"}`)
	}))
	defer srv.Close()

	b := New("app", "bot")
	b.apiBase = srv.URL
	src := &chanSource{ch: make(chan event, 8)}
	b.src = src

	// A gate Ask starts the reader under a per-drive ctx, then that ctx is cancelled.
	driveCtx, driveCancel := context.WithCancel(context.Background())
	b.startIntake(driveCtx)
	driveCancel()

	// Wait for the reader to observe cancellation and unlatch.
	deadline := time.Now().Add(3 * time.Second)
	for {
		b.mu.Lock()
		started := b.intakeStarted
		b.mu.Unlock()
		if !started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("intake never unlatched after its starting ctx was cancelled")
		}
		time.Sleep(time.Millisecond)
	}

	// A fresh Receive on a live ctx must revive the reader and deliver the message.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	src.ch <- event{Type: "events_api", EnvelopeID: "e1", Payload: json.RawMessage(`{"event":{"type":"message","text":"do it","user":"U1","channel":"C9"}}`)}
	tr, err := b.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive after restart: %v", err)
	}
	if tr.Goal != "do it" {
		t.Fatalf("got %+v, want goal 'do it' after intake restart", tr)
	}
}

// TestWSReadFrameRejectsHugeLength proves a hostile 64-bit frame length is rejected with
// an error instead of crashing the intake goroutine: a value > MaxInt64 goes NEGATIVE when
// cast to int and panics make([]byte, length); a merely huge one OOMs. readFrame must cap
// it before the make.
func TestWSReadFrameRejectsHugeLength(t *testing.T) {
	// 0x81 (FIN|text), 0x7F (unmasked, 127 = 64-bit length), then 8 bytes of 0xFF (as int
	// that is -1). Only 10 bytes: readFrame must bail before allocating any payload.
	frame := []byte{0x81, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	w := &wsConn{r: bufio.NewReader(bytes.NewReader(frame))}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("readFrame panicked on a hostile length instead of erroring: %v", r)
		}
	}()
	if _, _, err := w.readFrame(); err == nil {
		t.Fatal("expected an error for an oversized frame length")
	}
}

// TestSlackRetryAfterParsing pins the bounded backoff: a valid delta is honored, garbage
// falls back to the default, and an absurd value is capped.
func TestSlackRetryAfterParsing(t *testing.T) {
	if got := retryAfter("3"); got != 3*time.Second {
		t.Errorf("retryAfter(3) = %v, want 3s", got)
	}
	if got := retryAfter("garbage"); got != rateRetryDefault {
		t.Errorf("retryAfter(garbage) = %v, want default %v", got, rateRetryDefault)
	}
	if got := retryAfter("99999"); got != rateRetryMax {
		t.Errorf("retryAfter(99999) = %v, want cap %v", got, rateRetryMax)
	}
}

// TestSlackApiPostRetriesOn429 proves a rate-limited Web API call is retried (bounded) then
// succeeds, so a gate/ask/final message is not silently lost to a 429.
func TestSlackApiPostRetriesOn429(t *testing.T) {
	old := rateRetryDefault
	rateRetryDefault = time.Millisecond
	defer func() { rateRetryDefault = old }()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests) // no Retry-After → default (1ms in test)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	b := New("app", "bot")
	b.apiBase = srv.URL
	if err := b.Update(context.Background(), "C9", "hi"); err != nil {
		t.Fatalf("Update after a 429 retry: %v", err)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("server hit %d times, want 2 (one 429 + one success)", n)
	}
}
