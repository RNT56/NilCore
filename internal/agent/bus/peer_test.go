package bus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nilcore/internal/model"
)

// peerIface mirrors backend.Peer (internal/backend/native.go). We assert against a
// local copy rather than importing backend so the leaf bus package keeps a clean
// import graph; if backend.Peer changes, this drifts and the compile-time check
// below is what flags it.
type peerIface interface {
	Tools() []model.Tool
	Dispatch(ctx context.Context, name string, input json.RawMessage) (string, error)
}

// AgentPeer must satisfy the minimal handle the native loop expects (the whole
// point of P1-T03). A pointer receiver, matching native.go's *bus.AgentPeer note.
var _ peerIface = (*AgentPeer)(nil)

func newTestPeer(t *testing.T, b *Bus, self AgentID) *AgentPeer {
	t.Helper()
	p, err := NewPeer(b, self)
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	t.Cleanup(func() { b.Deregister(self) })
	return p
}

// Tools returns EXACTLY the three subagent tools and NOTHING that could command
// the cohort — no steer/cancel/spawn/delegate. This is the acceptance core: the
// surface itself is the authority asymmetry (I7).
func TestToolsExactlyThree(t *testing.T) {
	p := newTestPeer(t, New(nil, 4, 0), "sub")

	defs := p.Tools()
	if len(defs) != 3 {
		t.Fatalf("Tools() returned %d defs, want exactly 3", len(defs))
	}

	got := map[string]bool{}
	for _, d := range defs {
		got[d.Name] = true
	}
	for _, want := range []string{toolAskSupervisor, toolShareFinding, toolRequestReview} {
		if !got[want] {
			t.Errorf("Tools() missing %q", want)
		}
	}

	// No command-plane / spawn tool may EVER appear on a subagent's surface.
	forbidden := []string{"steer", "cancel", "spawn", "delegate", "spawn_subagent", "message_subagent"}
	for _, d := range defs {
		for _, bad := range forbidden {
			if strings.Contains(d.Name, bad) {
				t.Errorf("forbidden tool %q exposed to subagent (name=%q)", bad, d.Name)
			}
		}
		// Every def must be a well-formed object schema (the loop hands these to
		// the model verbatim).
		var schema struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(d.InputSchema, &schema); err != nil || schema.Type != "object" {
			t.Errorf("tool %q has malformed input schema: %v", d.Name, err)
		}
	}
}

// ask_supervisor routes to a blocking Bus.Ask: the supervisor's answer comes back,
// fenced, as the dispatch reply. A second goroutine plays the supervisor.
func TestDispatchAskSupervisorRoutesAndFences(t *testing.T) {
	b := New(nil, 4, 0)
	supIn, err := b.Register(Supervisor)
	if err != nil {
		t.Fatalf("register supervisor: %v", err)
	}
	defer b.Deregister(Supervisor)
	p := newTestPeer(t, b, "sub")

	const answer = "use net/http, no deps"
	go func() {
		q := <-supIn
		// The supervisor answers by correlation id; this is the path Bus.Ask waits on.
		_ = b.Send(context.Background(), Message{
			Sender:        string(Supervisor),
			To:            []AgentID{"sub"},
			Kind:          KindAnswer,
			CorrelationID: q.CorrelationID,
			Payload:       answer,
			TTL:           4,
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := p.Dispatch(ctx, toolAskSupervisor, mustInput(t, "question", "which router?"))
	if err != nil {
		t.Fatalf("Dispatch(ask_supervisor): %v", err)
	}
	if !strings.Contains(reply, answer) {
		t.Errorf("reply missing supervisor answer; got %q", reply)
	}
	// Fenced as data, not instructions (I7): the reply must carry the untrusted
	// boundary markers around the answer.
	if !strings.Contains(reply, "DATA ONLY") {
		t.Errorf("reply not guard.Wrapped (no fence marker); got %q", reply)
	}
}

// request_review routes to a blocking Ask too, on KindReviewRequest, and fences
// the KindReviewResult reply.
func TestDispatchRequestReviewRoutes(t *testing.T) {
	b := New(nil, 4, 0)
	supIn, err := b.Register(Supervisor)
	if err != nil {
		t.Fatalf("register supervisor: %v", err)
	}
	defer b.Deregister(Supervisor)
	p := newTestPeer(t, b, "sub")

	var gotKind Kind
	go func() {
		q := <-supIn
		gotKind = q.Kind
		_ = b.Send(context.Background(), Message{
			Sender:        string(Supervisor),
			To:            []AgentID{"sub"},
			Kind:          KindReviewResult,
			CorrelationID: q.CorrelationID,
			Payload:       "looks good",
			TTL:           4,
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := p.Dispatch(ctx, toolRequestReview, mustInput(t, "request", "review my diff"))
	if err != nil {
		t.Fatalf("Dispatch(request_review): %v", err)
	}
	if gotKind != KindReviewRequest {
		t.Errorf("request_review sent Kind %q, want %q", gotKind, KindReviewRequest)
	}
	if !strings.Contains(reply, "looks good") || !strings.Contains(reply, "DATA ONLY") {
		t.Errorf("review reply not routed/fenced; got %q", reply)
	}
}

// share_finding is async: it Sends a KindFinding and returns immediately with a
// fixed ack, awaiting no reply. The supervisor receives the finding in its mailbox.
func TestDispatchShareFindingAsync(t *testing.T) {
	b := New(nil, 4, 0)
	supIn, err := b.Register(Supervisor)
	if err != nil {
		t.Fatalf("register supervisor: %v", err)
	}
	defer b.Deregister(Supervisor)
	p := newTestPeer(t, b, "sub")

	ctx := context.Background()
	reply, err := p.Dispatch(ctx, toolShareFinding, mustInput(t, "finding", "tests pass"))
	if err != nil {
		t.Fatalf("Dispatch(share_finding): %v", err)
	}
	if !strings.Contains(reply, "shared") {
		t.Errorf("share_finding ack unexpected; got %q", reply)
	}

	select {
	case m := <-supIn:
		if m.Kind != KindFinding {
			t.Errorf("supervisor got Kind %q, want %q", m.Kind, KindFinding)
		}
		if !strings.Contains(m.Payload, "tests pass") {
			t.Errorf("finding payload missing content; got %q", m.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor never received the finding")
	}
}

// A blocking Ask whose supervisor never answers must NOT fail the step: Dispatch
// returns a graceful fenced "proceed" note on timeout, mirroring the advisor
// ceiling fallback in native.go.
func TestDispatchAskTimeoutGraceful(t *testing.T) {
	b := New(nil, 4, 0)
	// Register the supervisor but never answer.
	if _, err := b.Register(Supervisor); err != nil {
		t.Fatalf("register supervisor: %v", err)
	}
	defer b.Deregister(Supervisor)
	p := newTestPeer(t, b, "sub")

	// A short ctx deadline drives the timeout fast (Ask is ctx-bounded).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	reply, err := p.Dispatch(ctx, toolAskSupervisor, mustInput(t, "question", "anyone there?"))
	if err != nil {
		t.Fatalf("timeout should be graceful, not an error; got %v", err)
	}
	if !strings.Contains(reply, "best judgment") || !strings.Contains(reply, "DATA ONLY") {
		t.Errorf("timeout reply not a fenced proceed-note; got %q", reply)
	}
}

// An unknown tool name is a hard error — there is no steer/cancel/spawn route to
// fall through to, so a name outside the three is rejected, never silently routed.
func TestDispatchUnknownToolRejected(t *testing.T) {
	p := newTestPeer(t, New(nil, 4, 0), "sub")
	for _, name := range []string{"steer", "cancel", "spawn_subagent", "", "ask"} {
		if _, err := p.Dispatch(context.Background(), name, json.RawMessage(`{}`)); err == nil {
			t.Errorf("Dispatch(%q) = nil error, want rejection", name)
		}
	}
}

// Malformed / empty input is rejected before anything hits the bus, so an empty
// question or finding never travels.
func TestDispatchBadInput(t *testing.T) {
	p := newTestPeer(t, New(nil, 4, 0), "sub")
	cases := []struct {
		name  string
		tool  string
		input string
	}{
		{"not json", toolAskSupervisor, `{`},
		{"missing field", toolAskSupervisor, `{"q":"x"}`},
		{"empty value", toolShareFinding, `{"finding":""}`},
		{"wrong type", toolRequestReview, `{"request":123}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := p.Dispatch(context.Background(), c.tool, json.RawMessage(c.input)); err == nil {
				t.Errorf("Dispatch(%q,%s) = nil error, want rejection", c.tool, c.input)
			}
		})
	}
}

// NewPeer registers the mailbox and exposes it as In; a duplicate id is rejected so
// a live mailbox is never orphaned.
func TestNewPeerRegisters(t *testing.T) {
	b := New(nil, 4, 0)
	p, err := NewPeer(b, "sub")
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	defer b.Deregister("sub")
	if p.Self != "sub" || p.Bus != b || p.In == nil {
		t.Fatalf("NewPeer returned an incomplete handle: %+v", p)
	}
	if _, err := NewPeer(b, "sub"); err == nil {
		t.Error("NewPeer twice for the same id should fail")
	}
}

func mustInput(t *testing.T, field, value string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]string{field: value})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return raw
}
