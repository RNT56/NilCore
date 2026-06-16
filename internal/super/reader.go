package super

import (
	"context"
	"sync"

	"nilcore/internal/agent/bus"
	"nilcore/internal/eventlog"
)

// reader is the supervisor's DEDICATED bus-reader goroutine — the fix for the
// cross-facet deadlock (design risk #4, §3). It drains the supervisor's mailbox
// CONCURRENTLY with the supervisor's blocking primitives, so a subagent's blocking
// Bus.Ask (ask_supervisor / request_review) is answered even while the supervisor
// goroutine is parked inside await_results, doSpawn, or a long code turn.
//
// Without this, a subagent's Ask would hang to its ctx timeout whenever the
// supervisor is not itself draining its mailbox — serializing the cohort and
// burning wall-clock. With it, questions are answered promptly off the critical
// path, and async findings are queued for the supervisor loop to fold in between
// turns as fenced DATA (I7).
//
// Lifecycle: started in Run before any subagent can Ask; stopped (and the bus
// mailbox deregistered) on Run's defer. Delivery in Bus.Send is synchronous, so
// once Deregister has run no goroutine is left writing to the closed mailbox — no
// leak (design §3 "the one reader goroutine exits on Deregister").
type reader struct {
	sup   *Supervisor
	in    <-chan bus.Message
	stopc chan struct{}
	done  chan struct{}

	mu       sync.Mutex
	findings []string // async findings queued for the supervisor loop to drain
}

// startReader registers the supervisor's mailbox and launches the reader. It
// returns nil when no bus is wired (single-supervisor mode: no subagents talk
// back, so there is nothing to drain). The caller MUST defer reader.stop().
func (s *Supervisor) startReader(ctx context.Context) *reader {
	if s.Bus == nil {
		return nil
	}
	in, err := s.Bus.Register(bus.Supervisor)
	if err != nil {
		// The supervisor mailbox is a singleton; a registration failure means one is
		// already live (a misuse). Run without a reader rather than panic — every Ask
		// then falls back to the AgentPeer's graceful timeout, so the run is still
		// deadlock-free, just slower for any blocked subagent.
		s.Log.Append(eventlog.Event{Task: supervisorTask, Kind: "super_reader_skip",
			Detail: map[string]any{"reason": err.Error()}})
		return nil
	}
	r := &reader{sup: s, in: in, stopc: make(chan struct{}), done: make(chan struct{})}
	go r.loop(ctx)
	return r
}

// loop drains the mailbox until stop() or ctx cancellation. A blocking question or
// review-request is answered immediately on the bus (via the supervisor's Answer
// hook, with a graceful fallback); a finding is queued for the loop; steer/cancel
// to the supervisor are no-ops (the supervisor is the originator of those, never a
// recipient). It is the only goroutine that reads `in`.
func (r *reader) loop(ctx context.Context) {
	defer close(r.done)
	for {
		select {
		case <-r.stopc:
			return
		case <-ctx.Done():
			return
		case m, ok := <-r.in:
			if !ok {
				return // mailbox closed (Deregister): clean exit, no leak
			}
			r.handle(ctx, m)
		}
	}
}

// handle dispatches one inbound message. The body is already guard.Wrapped by the
// bus on delivery (I7); the reader reads only typed control fields (Kind /
// CorrelationID / Sender) to route it — it never parses the payload as an
// instruction.
func (r *reader) handle(ctx context.Context, m bus.Message) {
	switch m.Kind {
	case bus.KindQuestion, bus.KindReviewRequest:
		r.answer(ctx, m)
	case bus.KindFinding:
		r.mu.Lock()
		r.findings = append(r.findings, m.Payload)
		r.mu.Unlock()
		r.sup.Log.Append(eventlog.Event{Task: string(bus.Supervisor), Kind: "subagent_finding",
			Detail: map[string]any{"from": m.Sender}})
	default:
		// Heartbeat / answer / steer / cancel addressed to the supervisor: nothing to
		// do. The supervisor originates command-plane Kinds; it never obeys one.
	}
}

// answer replies to a blocking question/review-request on its CorrelationID so the
// asker's parked Bus.Ask resolves. The reply Kind matches the request
// (Question→Answer, ReviewRequest→ReviewResult); the body is the supervisor's
// Answer hook output, or a graceful "proceed with your best judgment" fallback so a
// subagent's Ask is ALWAYS answered promptly — never left to time out.
func (r *reader) answer(ctx context.Context, q bus.Message) {
	body := r.sup.answerBody(ctx, q)

	kind := bus.KindAnswer
	if q.Kind == bus.KindReviewRequest {
		kind = bus.KindReviewResult
	}
	// The reply is supervisor→subagent control-plane DATA. We send it (not Ask) on
	// the question's CorrelationID; the bus hands it straight to the parked waiter.
	_ = r.sup.Bus.Send(ctx, bus.Message{
		Sender:        string(bus.Supervisor),
		To:            []bus.AgentID{bus.AgentID(q.Sender)},
		Kind:          kind,
		CorrelationID: q.CorrelationID,
		Payload:       body,
		TTL:           8,
	})
	r.sup.Log.Append(eventlog.Event{Task: string(bus.Supervisor), Kind: "subagent_answer",
		Detail: map[string]any{"to": q.Sender, "kind": string(kind)}})
}

// takeFindings returns and clears the queued async findings. Called by the
// supervisor loop between turns (single reader → single consumer hand-off).
func (r *reader) takeFindings() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.findings) == 0 {
		return nil
	}
	out := r.findings
	r.findings = nil
	return out
}

// stop signals the reader to exit and deregisters the supervisor mailbox, then
// waits for the goroutine to finish — so no reader outlives Run (no goroutine
// leak). Idempotent-safe to call once via defer. nil-safe.
func (r *reader) stop() {
	if r == nil {
		return
	}
	close(r.stopc)
	r.sup.Bus.Deregister(bus.Supervisor) // closes `in`, unblocking the loop's receive
	<-r.done
}

// answerBody produces the supervisor's reply to a subagent question. It prefers the
// wired Answer hook; with none, it returns the graceful fallback the design
// mandates — so a blocking Ask never hangs waiting on a busy supervisor (design §3
// "every Ask ctx-bounded with a graceful 'no answer, proceed' fallback"). The reply
// is DATA the subagent treats as guidance, never an order (I7); the bus fences it
// on delivery and the peer fences it again at the tool_result seam.
func (s *Supervisor) answerBody(ctx context.Context, q bus.Message) string {
	if s.Answer != nil {
		// Load the grounded run-context snapshot (goal + plan + cohort + integration
		// tip) under snapMu and hand it to Answer BY VALUE, so the reply is grounded in
		// the supervisor's own plan and the cohort's actual state. loadRunContext is a
		// non-blocking mutex copy — it never touches the parked main goroutine and can
		// never hang, so deadlock-freedom is untouched.
		if body := s.Answer(ctx, q, s.loadRunContext()); body != "" {
			return body
		}
	}
	return "No specific steer available right now — proceed with your best judgment within your task's scope, " +
		"or call finish and let the verifier and integration decide. Do not exceed your scope."
}
