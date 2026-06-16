package super

import (
	"context"
	"strings"
	"testing"
	"time"

	"nilcore/internal/agent/bus"
	"nilcore/internal/model"
	"nilcore/internal/roster"
	"nilcore/internal/spawn"
)

// TestSubagentAskAnsweredWhileSupervisorBusy is the deadlock-freedom acceptance
// test (design risk #4, §3): a subagent's BLOCKING Bus.Ask must be answered even
// while the supervisor goroutine is parked inside a spawn/code turn. The dedicated
// reader goroutine drains the supervisor's mailbox concurrently, so the parked
// supervisor never starves the asker.
//
// We model "the supervisor is busy" by having the SpawnFunc itself issue a real
// blocking ask from a subagent peer (exactly what an implementer's ask_supervisor
// tool does mid-task) and require the answer to arrive promptly — far inside the
// Ask's own ctx deadline. If the reader were missing, this Ask would hang until
// its timeout and the test would observe the graceful-fallback note instead of the
// supervisor's real answer (and take the full timeout to do so).
func TestSubagentAskAnsweredWhileSupervisorBusy(t *testing.T) {
	b := bus.New(nil, 4, 0)

	const answer = "use stdlib net/http, no deps (I6)"

	// The SpawnFunc plays the subagent: it registers a peer, asks the supervisor a
	// blocking question, and reports the fenced reply back in its summary so the
	// test can assert the real answer (not the timeout fallback) was received.
	var asked = make(chan struct{})
	spawnFn := func(ctx context.Context, spec SubagentSpec) spawn.Result {
		if _, err := bus.NewPeer(b, bus.AgentID(spec.ID)); err != nil {
			return spawn.Result{ID: spec.ID, Err: err}
		}
		defer b.Deregister(bus.AgentID(spec.ID))

		askCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		close(asked)
		reply, err := b.Ask(askCtx, bus.Message{
			Sender:  spec.ID,
			To:      []bus.AgentID{bus.Supervisor},
			Kind:    bus.KindQuestion,
			Payload: "which router library should I use?",
			TTL:     8,
		})
		if err != nil {
			// A hang would surface here as a timeout — the failure mode the reader fixes.
			return spawn.Result{ID: spec.ID, Err: err}
		}
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID, Summary: reply.Payload}
	}

	m := &scriptModel{responses: []model.Response{
		textResp(toolUse("u1", "spawn_subagent", SubagentSpec{ID: "super.t1", Role: roster.RoleImplementer, Goal: "health handler"})),
		textResp(toolUse("u2", "finish", map[string]string{"summary": "done"})),
	}}
	s := baseSup(m, passVerifier{})
	s.Bus = b
	s.Spawn = spawnFn
	// The supervisor answers the subagent's question with a concrete steer. This
	// runs ON THE READER GOROUTINE while s.Run is parked inside spawnFn's Ask.
	s.Answer = func(_ context.Context, q bus.Message, _ RunContext) string {
		if q.Kind != bus.KindQuestion {
			t.Errorf("answer hook saw kind %q, want question", q.Kind)
		}
		return answer
	}

	// A short overall deadline: if the Ask hung, Run would not finish here.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan Outcome, 1)
	go func() {
		out, err := s.Run(ctx, "goal")
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- out
	}()

	select {
	case out := <-done:
		// The implementer's summary must carry the supervisor's REAL answer — proof
		// the Ask resolved via the reader, not the timeout fallback.
		var got string
		for _, c := range allToolResults(m.lastMsgs) {
			if strings.Contains(c, answer) {
				got = answer
			}
		}
		if got != answer {
			t.Fatalf("subagent never received the supervisor's answer %q (reader did not drain)", answer)
		}
		_ = out
	case <-time.After(8 * time.Second):
		t.Fatal("Run hung: a subagent Ask was not answered while the supervisor was busy")
	}
}

// TestReaderAnswersWithFallbackWhenNoHook asserts the graceful fallback: with no
// Answer hook wired, a subagent's blocking Ask is STILL answered promptly (with
// the "proceed with your best judgment" note) rather than hanging to its timeout.
func TestReaderAnswersWithFallbackWhenNoHook(t *testing.T) {
	b := bus.New(nil, 4, 0)

	var replyPayload string
	spawnFn := func(ctx context.Context, spec SubagentSpec) spawn.Result {
		_, err := bus.NewPeer(b, bus.AgentID(spec.ID))
		if err != nil {
			return spawn.Result{ID: spec.ID, Err: err}
		}
		defer b.Deregister(bus.AgentID(spec.ID))
		askCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		reply, err := b.Ask(askCtx, bus.Message{
			Sender: spec.ID, To: []bus.AgentID{bus.Supervisor},
			Kind: bus.KindQuestion, Payload: "blocked", TTL: 8,
		})
		if err != nil {
			return spawn.Result{ID: spec.ID, Err: err}
		}
		replyPayload = reply.Payload
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
	}

	m := &scriptModel{responses: []model.Response{
		textResp(toolUse("u1", "spawn_subagent", SubagentSpec{ID: "super.t1", Role: roster.RoleImplementer, Goal: "x"})),
		textResp(toolUse("u2", "finish", map[string]string{"summary": "done"})),
	}}
	s := baseSup(m, passVerifier{})
	s.Bus = b
	s.Spawn = spawnFn
	// No Answer hook → the reader replies with the graceful fallback.

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Run(ctx, "goal"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(replyPayload, "best judgment") {
		t.Errorf("fallback answer not delivered; got %q", replyPayload)
	}
}

// TestReaderNoLeakWithoutBus checks the nil-bus path: Run starts no reader and
// returns cleanly (single-supervisor mode), so the reader is never a hard dep.
func TestReaderNoLeakWithoutBus(t *testing.T) {
	m := &scriptModel{responses: []model.Response{
		textResp(toolUse("u1", "finish", map[string]string{"summary": "done"})),
	}}
	s := &Supervisor{Model: m, Verify: passVerifier{}.Check, MaxRounds: 3, MaxDepth: 1}
	out, err := s.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done {
		t.Error("nil-bus run should still converge")
	}
}
