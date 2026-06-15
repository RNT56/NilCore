// Package server is NilCore's long-running serve mode (P1-T07, refit C3-T02): it
// listens on a chat channel and gives every thread the SAME conversational
// Session the terminal front door uses, so Telegram/Slack get queue+steer and
// auto-routing for free.
//
// The shape (docs/CONVERSATIONAL.md §5.4 / §7): a single intake goroutine reads
// the channel and, for each inbound message, finds-or-creates that thread's
// session.Session and calls Session.Turn. A NEW thread (or a message to an Idle
// thread) starts a drive; a message to a thread whose Session is Working becomes a
// QUEUE (a plain message) or a STEER (a '!'/'/steer'-marked message) — the
// queue-vs-steer classification lives inside the Session (classifyInterrupt), so
// serve and chat share one rule. Turn returns immediately while the drive runs in
// its own goroutine, so the intake loop keeps accepting messages mid-drive (the
// concurrent intake the prior one-task-at-a-time server could not do).
//
// The single load-bearing trust line (I7 / P2-T07): channel.Authorized.Permit
// gates EVERY message BEFORE it is promoted to a principal Turn. An unauthorized
// sender's queue/steer is refused (logged + the sender told) and never reaches
// Turn — a steer is an un-guard.Wrap'd principal instruction, so admitting one
// from an unauthorized sender would inject controlling instructions. Authorization
// at the channel boundary is the ONLY promotion to principal trust. A thread's
// Session pins its Sender from the FIRST authorized message; a later message from a
// DIFFERENT (still-authorized) sender to the same thread is refused too, so one
// thread is one principal's conversation.
//
// Each Session's reasoning/intent Emitter is a thin adapter over Channel.Update so
// live progress streams to that thread; gates still route through Channel.Ask via
// the per-thread policy.Approver the wiring supplies. The channel-specific concerns
// (Permit, Update, Ask) live here; the heavy machinery wiring (router + drivers +
// metered provider + budget) is injected as a SessionFactory the cmd layer builds.
package server

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"nilcore/internal/channel"
	"nilcore/internal/emit"
	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
	"nilcore/internal/session"
)

// SessionFactory builds a fully-wired conversational Session for one thread. The
// cmd layer supplies it (it owns the router, drivers, metered provider, and
// conversation-scoped budget). The server hands in the transport-bound pieces it
// owns — the threadID (used as the conversation/budget key), the pinned sender,
// the Emitter that streams reasoning back to this thread, and the Approver that
// routes gates back to this thread — so the factory stays pure machinery assembly
// and the trust/transport concerns stay in the server.
type SessionFactory func(ctx context.Context, threadID, sender string, out emit.Emitter, approver policy.Approver) *session.Session

// Authorizer is the per-message trust gate: Permit reports whether a principal may
// command the agent. *channel.Authorized satisfies it. It is the ONLY promotion to
// principal trust — every inbound message is Permit-checked before it can become a
// Turn.
type Authorizer interface {
	Permit(principal string) bool
}

// Server gives each channel thread its own conversational Session. It owns the
// per-thread Session map, the concurrent intake loop, and the channel-specific
// Emitter/Approver wiring; the Session machinery is injected via NewSession.
type Server struct {
	Channel    channel.Channel // the transport (already Authorized-wrapped by the cmd layer)
	Auth       Authorizer      // per-message Permit gate; nil ⇒ deny-all (no ambient authority)
	NewSession SessionFactory  // builds a wired Session per thread; required
	Log        *eventlog.Log   // append-only audit; Append is nil-safe

	mu      sync.Mutex         // guards threads
	threads map[string]*thread // threadID → conversation + its surface sink
}

// thread is one channel conversation: the wired Session and the per-thread surface
// sink (the channelEmitter whose sender goroutine the server joins at shutdown).
type thread struct {
	sess *session.Session
	emit *channelEmitter
}

// Serve runs the listen→route loop until ctx is cancelled. It returns nil on a
// clean shutdown; transient channel errors are logged and the loop continues. On
// shutdown it waits for every thread's in-flight drive to unwind on the cancelled
// ctx (no abandoned drive goroutine, no torn worktree) before returning.
//
// Receive runs in the caller's goroutine (the serve loop); each accepted message's
// Turn launches its drive in a background goroutine and returns at once, so the
// next Receive happens WHILE a drive runs — that is the concurrent intake the
// refit adds. There is no separate long-lived intake goroutine to leak: the serve
// loop IS the intake, and it exits when ctx is cancelled or Receive returns a
// non-transient error.
func (s *Server) Serve(ctx context.Context) error {
	if s.NewSession == nil {
		return fmt.Errorf("server: NewSession factory is required")
	}
	s.Log.Append(eventlog.Event{Kind: "serve_start"})

	for {
		req, err := s.Channel.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				s.drainShutdown()
				s.Log.Append(eventlog.Event{Kind: "serve_stop"})
				return nil
			}
			s.Log.Append(eventlog.Event{Kind: "serve_error", Detail: map[string]any{"error": err.Error()}})
			continue
		}
		s.intake(ctx, req)
	}
}

// intake handles ONE inbound message: it enforces the trust line, then routes the
// message to its thread's Session. It returns promptly — a new drive is launched in
// its own goroutine inside Session.Turn (Idle) or the message is pushed to the
// running loop's inbox (Working); either way the serve loop is free to Receive the
// next message immediately.
func (s *Server) intake(ctx context.Context, req channel.TaskRequest) {
	// TRUST LINE (I7 / P2-T07): Permit BEFORE anything. An unauthorized sender's
	// message — queue OR steer — is refused here and never reaches Turn, so it can
	// never be promoted to a principal (un-guard.Wrap'd) instruction. A nil Auth is
	// deny-all (no ambient authority): the cmd layer always wires one.
	if s.Auth == nil || !s.Auth.Permit(req.Sender) {
		s.Log.Append(eventlog.Event{Kind: "unauthorized_command",
			Detail: map[string]any{"sender": req.Sender, "thread": req.ThreadID}})
		_ = s.Channel.Update(ctx, req.ThreadID, "Unauthorized: you are not permitted to command this agent.")
		return
	}

	th, created, ok := s.threadFor(ctx, req.ThreadID, req.Sender)
	if !ok {
		// A second, different (still-authorized) sender reached a thread already
		// pinned to another principal. One thread is one principal's conversation, so
		// the foreign message is refused — never folded into someone else's Session.
		s.Log.Append(eventlog.Event{Kind: "unauthorized_command",
			Detail: map[string]any{"sender": req.Sender, "thread": req.ThreadID, "reason": "sender_mismatch"}})
		_ = s.Channel.Update(ctx, req.ThreadID, "Unauthorized: this conversation is owned by another principal.")
		return
	}
	if created {
		s.Log.Append(eventlog.Event{Task: req.ThreadID, Kind: "session_open",
			Detail: map[string]any{"sender": req.Sender, "thread": req.ThreadID}})
		_ = s.Channel.Update(ctx, req.ThreadID, "Starting: "+req.Goal)
	}

	// A /cancel (or /stop) from the principal aborts the in-flight run but keeps the
	// conversation — it is NOT a Turn (never folded as queue/steer). Run it off the
	// serve loop so a slow-to-unwind drive cannot block intake of other threads.
	if isCancelCommand(req.Goal) {
		thread := th
		go func() {
			if thread.sess.Cancel() {
				_ = s.Channel.Update(ctx, req.ThreadID, "Cancelled the current run.")
			} else {
				_ = s.Channel.Update(ctx, req.ThreadID, "Nothing is running.")
			}
		}()
		return
	}

	// Turn is the single principal entry point: Idle ⇒ route + launch a drive;
	// Working ⇒ queue/steer into the running loop. It returns immediately either
	// way, so intake (and the serve loop) never blocks until a drive completes.
	if err := th.sess.Turn(ctx, req.Goal); err != nil {
		if ctx.Err() != nil {
			return // shutting down; the drive unwinds on the cancelled ctx
		}
		_ = s.Channel.Update(ctx, req.ThreadID, "Failed to route: "+err.Error())
	}
}

// isCancelCommand reports whether a message is the abort-the-run control verb
// (mirrors the chat REPL's /cancel and /stop) rather than a task / queue / steer.
func isCancelCommand(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	return t == "/cancel" || t == "/stop"
}

// threadFor finds-or-creates the thread (Session + surface sink) under s.mu. On
// first contact it builds a wired Session via NewSession with this thread's Emitter
// (→Update) and Approver (→Ask), pinning Sender. It returns (thread, created, ok):
// ok is false when the thread already belongs to a DIFFERENT principal (the caller
// refuses the message). The map write and the pin happen under the lock so two
// near-simultaneous first messages cannot create two Sessions for one thread.
func (s *Server) threadFor(ctx context.Context, threadID, sender string) (th *thread, created, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.threads == nil {
		s.threads = make(map[string]*thread)
	}
	if existing := s.threads[threadID]; existing != nil {
		if existing.sess.Sender != sender {
			return nil, false, false
		}
		return existing, false, true
	}

	em := &channelEmitter{ctx: ctx, ch: s.Channel, thread: threadID}
	approver := channel.NewApprover(ctx, s.Channel, threadID)

	sess := s.NewSession(ctx, threadID, sender, em, approver)
	th = &thread{sess: sess, emit: em}
	s.threads[threadID] = th
	return th, true, true
}

// drainShutdown waits, after ctx is cancelled, for every thread's in-flight drive
// to unwind AND its surface-sender goroutine to exit, so Serve does not return (and
// the process exit) while a drive is still unwinding mid-write or a sender goroutine
// is still live. Session.Wait joins the drive goroutine; channelEmitter.wait joins
// the sender goroutine (which exits on the cancelled ctx). With nothing running both
// return at once. No goroutine outlives Serve.
func (s *Server) drainShutdown() {
	s.mu.Lock()
	live := make([]*thread, 0, len(s.threads))
	for _, th := range s.threads {
		live = append(live, th)
	}
	s.mu.Unlock()
	for _, th := range live {
		th.sess.Wait()
		th.emit.wait()
	}
}

// channelEmitter is the per-thread reasoning sink: an emit.Emitter that streams
// the loop's live intent back to one channel thread via Channel.Update. It is the
// serve-mode counterpart of chat's WriterEmitter (which writes to stdout).
//
// The sink is NON-BLOCKING from the loop's perspective (docs/CONVERSATIONAL.md
// §5.2): Channel.Update is a remote HTTP call (Telegram/Slack rate limits, 429
// retry-after) and the loop must never block on it — the steer that unblocks the
// loop must always be deliverable. So Emit only enqueues onto a buffered,
// drop-oldest channel; a dedicated per-thread sender goroutine drains it and makes
// the actual Update call off the loop's critical path. A full buffer drops the
// OLDEST pending line (coalescing under backpressure) rather than blocking the
// loop or unboundedly growing — the latest intent is the one worth showing.
//
// Lifecycle: the sender goroutine starts on the first Emit and exits when the serve
// ctx is cancelled (shutdown) — bounded, joinable via the WaitGroup, no leak. Emit
// is safe for concurrent calls from the loop goroutine (sync.Once guards the lazy
// start; the channel send/drain is inherently concurrent-safe).
type channelEmitter struct {
	ctx    context.Context
	ch     channel.Channel
	thread string

	once sync.Once
	q    chan string
	wg   sync.WaitGroup
}

// emitBuffer bounds the per-thread surface queue. Small on purpose: under
// backpressure the loop races ahead of a rate-limited channel and we coalesce to
// the freshest few lines rather than backlog stale ones.
const emitBuffer = 32

// Emit enqueues one rendered surface line for this thread without ever blocking the
// caller (the loop goroutine). It lazily starts the per-thread sender goroutine on
// first use. When the buffer is full it drops the oldest pending line and enqueues
// the new one (drop-oldest coalescing) — the loop stays free to steer.
func (e *channelEmitter) Emit(ev emit.Event) {
	e.once.Do(e.start)
	line := surfaceLine(ev)
	for {
		select {
		case e.q <- line:
			return
		default:
			// Full: discard the oldest pending line, then retry the enqueue. The
			// drain is non-blocking too, so this can never spin against a live drainer.
			select {
			case <-e.q:
			default:
			}
		}
	}
}

// start launches the per-thread sender goroutine. It drains the queue and performs
// the blocking Channel.Update off the loop's path, exiting cleanly when the serve
// ctx is cancelled — so the goroutine is bounded by the serve lifetime and joined
// at shutdown (no leak).
func (e *channelEmitter) start() {
	e.q = make(chan string, emitBuffer)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			select {
			case <-e.ctx.Done():
				return
			case line := <-e.q:
				_ = e.ch.Update(e.ctx, e.thread, line)
			}
		}
	}()
}

// wait blocks until the per-thread sender goroutine has exited. It is safe to call
// when no goroutine was ever started (Emit never ran): the WaitGroup count is 0 and
// it returns at once. Called only at shutdown, after the serve ctx is cancelled (so
// the goroutine is already on its way out).
func (e *channelEmitter) wait() {
	e.wg.Wait()
}

// surfaceLine renders one emit.Event as a single progress line for the channel.
// It surfaces the harness-authored intent text (already metadata-light), never a
// raw model/tool dump, so laundered tool output cannot ride into the thread
// verbatim (docs/CONVERSATIONAL.md §5.2). An empty body collapses to the bare
// kind tag so a thread never receives a blank message.
func surfaceLine(e emit.Event) string {
	switch e.Kind {
	case emit.KindIntent:
		if e.Text == "" {
			return "…"
		}
		return e.Text
	case emit.KindTool:
		return "→ " + e.Text
	case emit.KindVerify:
		return "✓ " + e.Text
	case emit.KindSteerAck:
		return "! " + e.Text
	default:
		if e.Text == "" {
			return e.Kind
		}
		return e.Text
	}
}
