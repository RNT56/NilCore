package session

import (
	"context"
	"errors"
	"strings"
	"sync"

	"nilcore/internal/ask"
	"nilcore/internal/backend"
	"nilcore/internal/budget"
	"nilcore/internal/emit"
	"nilcore/internal/eventlog"
	"nilcore/internal/inbox"
	"nilcore/internal/memory"
	"nilcore/internal/model"
	"nilcore/internal/summarize"
)

// Routing-failure sentinels: an Idle Turn that cannot reach a router or a wired
// driver returns one of these and leaves the Session in Idle (never wedged in
// Routing, never panicking). They let the core run with fakes before the full
// Router/Drivers machinery (C2-T02/C2-T03) is injected.
var (
	errNoRouter = errors.New("session: no router configured")
	errNoDriver = errors.New("session: no driver for route")
)

// Session is the persistent state container for one conversation. It holds the
// canonical turn History, the bounded WorkState carry-over, the user→agent Inbox
// a running drive drains, and a Phase machine that makes the Idle→Working→Idle
// spine re-enterable. Turn is the single entry point; Phase and History are the
// only shared mutable state and are guarded by mu, so a Turn caller never races
// the drive goroutine.
type Session struct {
	ID     string // "chat-local" or the serve threadID; the conversation/budget key
	Sender string // pinned from the FIRST authorized request
	Repo   string // working repository path

	// Router and Drivers are injected seams (C2-T02/C2-T03). A nil Router or a
	// missing driver makes an Idle Turn a no-op routing failure rather than a
	// panic, so the core is usable with fakes before that machinery lands.
	Router  Router
	Drivers Drivers

	Out emit.Emitter // reasoning/intent sink (terminal stdout / channel); nil-safe

	// Notify, if set, is called once with the TERMINAL outcome of every WORK drive
	// (native/supervise/project — not a plain chat reply), AFTER the fold. It is how a
	// DETACHED principal learns a long job finished (done / stopped / error) without
	// re-attaching: the serve front door wires it to push a one-line status to the
	// owning channel thread over a durable ctx. nil ⇒ no-op (the attached terminal/chat
	// path already shows the result live), so it is byte-identical when unwired. It
	// runs on the drive goroutine after Phase returned to Idle — best-effort; a slow
	// push must not wedge the conversation, so the wired closure bounds its own call.
	Notify func(Notification)

	Log    *eventlog.Log  // append-only audit; Append is nil-safe
	Mem    *memory.Memory // optional, nil-safe
	Budget *budget.Ledger // CONVERSATION-scoped ledger (keyed by ID)

	// Store is the optional persistence seam (C4-T01): the bounded WorkState is
	// restored at Restore and written back on each terminal drive so a
	// conversation continues across a restart. A nil Store is in-memory only and
	// never blocks (best-effort). It is *agent.Checkpoint in production; a fake in
	// tests. See persist.go.
	Store Store

	// Inbox is the user→agent seam a running drive drains. It is created at New
	// and reused across drives so a mid-work Turn always has somewhere to push.
	Inbox *inbox.Box

	// askBox is the outbound (loop→user) clarification seam — non-nil ONLY when an
	// interactive front door enabled attended asking (EnableAskUser). It is the mirror
	// of Inbox: a parked ask_user drive collects answers through it, and a Turn arriving
	// while Phase==AwaitingInput resolves the current question with it. Headless
	// sessions leave it nil, so ask_user is never wired and never blocks.
	askBox *ask.Box

	// Sizer is the native-vs-supervise sizing heuristic used ONLY when the user has
	// pinned ModeExecute (which bypasses the auto-router): a complex goal routes to
	// the supervisor, otherwise the single native loop. It is the SAME pure function
	// the SupervisorFirstRouter holds (chatShouldSupervise), injected here so the
	// session need not depend on the concrete router type. nil ⇒ "not complex" (the
	// conservative single-loop default). Unused in ModeAuto (the router sizes there).
	Sizer func(goal string) bool

	// readRoots are additional READ-ONLY context roots (absolute, symlink-resolved
	// by the caller before AddReadRoot) the drive's read/search tools may consult
	// beyond the worktree — the user's "add a folder/files as context" (X-T01).
	// Guarded by mu; threaded into each drive's DriveInput at launch. They are never
	// writable (only the read/search tools consult them), so they cannot widen the
	// single-writable-root invariant (I4).
	readRoots []string

	// CtxWindow resolves a model id to its context-window size in tokens (injected
	// from meter.CtxWindow so this leaf does not import meter). nil ⇒ no gauge and no
	// auto-compaction. Summarizer is the metered provider auto-compaction summarizes
	// History with when the window nears full; nil ⇒ compaction off (byte-identical).
	CtxWindow  func(modelID string) int
	Summarizer model.Provider
	usage      usageState // latest model usage (the context-gauge signal); guarded by mu

	mu      sync.Mutex      // guards Phase + History + driveCancel + gatePending
	Phase   Phase           // current conversational state
	History []model.Message // canonical turns — the shape native/super build
	State   WorkState       // bounded carry-over (never raw transcripts)
	drives  sync.WaitGroup  // in-flight drive tracker; incremented UNDER mu at the Routing flip (not at launch), so a concurrent Cancel during Routing always observes a positive counter before Wait — see Turn/toIdle/drive

	// gateReply/gatePending are the chat front door's yes/no rendezvous for an
	// irreversible-action gate (gate.go): the session-backed approver parks the drive in
	// AwaitingGate and blocks on gateReply; a typed Turn while AwaitingGate resolves it.
	// This makes AwaitingGate a real phase and lets the chat REPL keep ONE stdin reader
	// (no ConsoleApprover racing it). nil/false on a session that never wires the
	// session gate approver (serve uses Channel.Ask; the TUI uses its modal approver).
	gateReply   chan string
	gatePending bool

	// driveCancel cancels the CURRENT drive's context — the Routing model call and
	// the running loop — so Cancel() aborts the in-flight run while leaving the
	// conversation alive. Set under mu when a drive launches; cleared on every
	// return to Idle. nil ⇒ no drive in flight.
	driveCancel context.CancelFunc
}

// Drivers is the route→machine table the Session launches into. Each field is an
// injected Driver (C2-T03); a nil field for a chosen route is a routing failure,
// logged and returned to Idle, never a panic.
type Drivers struct {
	Native    Driver
	Supervise Driver
	Project   Driver
	Chat      Driver
}

// New constructs a Session in the Idle phase with a fresh Inbox bound to the
// conversation ID (the inbox's audit label). The caller wires Router/Drivers/Out
// and the metered Budget; History and State start empty (a fresh conversation).
func New(id, sender, repo string, log *eventlog.Log) *Session {
	return &Session{
		ID:        id,
		Sender:    sender,
		Repo:      repo,
		Log:       log,
		Inbox:     inbox.New(log, id),
		Phase:     Idle,
		gateReply: make(chan string, 1),
	}
}

// Turn is the SINGLE entry point: every principal message flows through it.
//
// Under s.mu it reads Phase and decides exactly one of two paths:
//
//   - Phase is in-flight (Working/Routing/AwaitingGate): the message is a
//     follow-up to a running drive. The user turn is appended to History, a
//     metadata-only session_followup is logged, and the message is pushed to the
//     Inbox with the locally-classified mode (queue by default; steer only on a
//     prefix mark). The running loop drains it; Turn returns immediately.
//
//   - Phase is Idle: there is no drive to follow up. Turn asks the injected
//     Router which machine to run, logs session_route, launches the chosen Driver
//     in a goroutine wired with the Inbox, and sets Phase to Working. On the
//     drive's completion the goroutine folds the terminal result into State and
//     returns Phase to Idle.
//
// The routing/launch path releases s.mu across the (possibly blocking) Router
// call so a concurrent steer Turn stays responsive: once Phase is Routing a
// racing Turn takes the in-flight branch and pushes to the Inbox.
func (s *Session) Turn(ctx context.Context, text string) error {
	userMsg := userTurn(text)

	s.mu.Lock()
	if s.Phase != Idle {
		phase := s.Phase
		askBox := s.askBox
		s.History = append(s.History, userMsg)
		s.mu.Unlock()

		// AwaitingInput: the drive is parked on an ask_user question, so this typed
		// line IS the answer — route it to the ask box, NOT the queue/steer inbox
		// (classifyInterrupt is bypassed, so an answer that begins with '!' is never
		// mis-read as a steer). To redirect instead of answering, the operator uses
		// /cancel (a control verb the front door peels off before Turn). If the batch
		// just ended between the phase read and Resolve, the line falls through to the
		// normal follow-up path below so it is never lost.
		if phase == AwaitingInput && askBox != nil && askBox.Resolve(text) {
			s.Log.Append(eventlog.Event{Task: s.ID, Kind: "session_answer",
				Detail: map[string]any{"phase": phase.String()}})
			return nil
		}

		// AwaitingGate: the drive is parked on the session-backed approver, so this
		// typed line is the y/n gate answer — routed to the gate rendezvous, never the
		// queue/steer inbox or the ask box (the explicit phase predicate). A non-y/n
		// line re-prompts inside approveViaTurn. If the gate just resolved, it falls
		// through to the normal follow-up.
		if phase == AwaitingGate && s.resolveGate(text) {
			s.Log.Append(eventlog.Event{Task: s.ID, Kind: "session_gate_reply",
				Detail: map[string]any{"phase": phase.String()}})
			return nil
		}

		// In-flight: fold the follow-up into the running loop's Inbox (queue/steer).
		mode := classifyInterrupt(text)
		s.Log.Append(eventlog.Event{
			Task: s.ID,
			Kind: "session_followup",
			Detail: map[string]any{
				"mode":  mode.String(),
				"phase": phase.String(),
			},
		})
		s.Inbox.Push(userMsg, mode)
		return nil
	}

	// Idle: this Turn starts a new drive. Append the turn and claim the work by
	// flipping to Routing under the lock, so a concurrent Turn sees in-flight and
	// pushes to the Inbox instead of racing into a second launch.
	s.History = append(s.History, userMsg)
	s.Phase = Routing
	// Track the drive from the Routing flip, UNDER mu — not at launch. A concurrent
	// Cancel acquires mu only after this unlock, so it always observes a positive
	// counter before drives.Wait(); the matching Done() fires either in drive() (the
	// launched path) or in toIdle() (every route/launch failure path). This closes
	// the Add-vs-Wait race a Cancel during the Routing window could otherwise hit.
	s.drives.Add(1)
	st := s.State
	history := s.snapshotHistory()
	// Wrap the conversation ctx in a per-drive cancellable context so Cancel() can
	// abort THIS run (the routing call + the loop) without tearing down the
	// conversation. Cleared on return to Idle (drive completion / routing failure).
	driveCtx, cancel := context.WithCancel(ctx)
	s.driveCancel = cancel
	s.mu.Unlock()

	return s.route(driveCtx, text, st, history)
}

// Cancel aborts the in-flight drive (if any) and waits for it to unwind, leaving
// the conversation in Idle so the principal can immediately issue a new
// instruction. It is the third mid-work operation alongside queue and steer:
// queue folds at the next step, steer pauses-and-reconsiders, Cancel STOPS the
// current run. The loops honor the cancelled drive context — the in-flight model
// call (and any sandboxed tool/exec under it) unwinds to a clean interrupted
// result and the throwaway worktree is discarded. Returns true if a run was
// cancelled, false if the session was already Idle.
func (s *Session) Cancel() bool {
	s.mu.Lock()
	if s.Phase == Idle || s.driveCancel == nil {
		s.mu.Unlock()
		return false
	}
	cancel := s.driveCancel
	s.mu.Unlock()

	s.Log.Append(eventlog.Event{Task: s.ID, Kind: "session_cancel"})
	cancel()        // cancel the drive ctx → the loop unwinds to a clean interrupt
	s.drives.Wait() // block until the drive goroutine has returned to Idle
	return true
}

// clearDriveCancelLocked releases the current drive's cancel func; it is called
// with s.mu held on every return to Idle. Calling the func after the drive has
// already ended is a harmless no-op that frees the context's resources.
func (s *Session) clearDriveCancelLocked() {
	if s.driveCancel != nil {
		s.driveCancel()
		s.driveCancel = nil
	}
}

// route runs the injected Router, logs the decision, and launches the chosen
// driver. It runs with s.mu released (Router.Route may make a model call); on any
// routing failure it returns the Session to Idle so the conversation is not
// wedged in Routing.
func (s *Session) route(ctx context.Context, text string, st WorkState, history []model.Message) error {
	// Auto-compaction: if the context window is near full, summarize the prior
	// conversation into a compact seed before launching, so a long conversation
	// continues rather than overrunning the window. No-op unless a Summarizer is
	// wired and the window is ≥ the threshold (so fake-driven paths are unchanged).
	history = s.maybeCompact(ctx, st, history)

	// A pinned mode OVERRIDES the auto-router: the user has declared intent, so it
	// governs which machine runs (and the read-only modes pin the capability the
	// driver builds). ModeAuto falls through to the classifier exactly as before, so
	// an unset mode is byte-identical. The mode route needs no model call.
	if r, ok := s.routeForMode(st.Mode, text); ok {
		s.Log.Append(eventlog.Event{
			Task:   s.ID,
			Kind:   "session_route",
			Detail: map[string]any{"route": r.String(), "mode": st.Mode.String(), "len_text": len(text)},
		})
		return s.launch(ctx, r, text, st, history)
	}

	if s.Router == nil {
		s.toIdle()
		return errNoRouter
	}

	r, err := s.Router.Route(ctx, text, st)
	if err != nil {
		s.Log.Append(eventlog.Event{
			Task:   s.ID,
			Kind:   "session_route",
			Detail: map[string]any{"error": true},
		})
		s.toIdle()
		return err
	}

	s.Log.Append(eventlog.Event{
		Task:   s.ID,
		Kind:   "session_route",
		Detail: map[string]any{"route": r.String(), "len_text": len(text)},
	})
	return s.launch(ctx, r, text, st, history)
}

// routeForMode maps a pinned Mode to a Route, bypassing the auto-router. The
// read-only modes (Discuss/Plan) run the single native loop with read-only
// capability (built by the driver from DriveInput.Mode); Execute sizes
// native-vs-supervise via the injected Sizer (the same heuristic the router uses),
// so a large execute request still fans out. ModeAuto returns ok=false so the
// caller falls through to the classifier — the byte-identical default.
func (s *Session) routeForMode(mode Mode, text string) (Route, bool) {
	switch mode {
	case ModeDiscuss, ModePlan:
		return RouteNative, true
	case ModeExecute:
		if s.Sizer != nil && s.Sizer(text) {
			return RouteSupervise, true
		}
		return RouteNative, true
	default:
		return RouteContinue, false
	}
}

// launch resolves the Route to a driver, claims Working, and starts the drive
// goroutine. It is shared by the mode-override path and the auto-router path so
// the DriveInput (which now carries the pinned Mode) and the Working/Active
// bookkeeping are built one way. A route with no wired driver returns the Session
// to Idle with errNoDriver rather than panicking.
func (s *Session) launch(ctx context.Context, r Route, text string, st WorkState, history []model.Message) error {
	drv := s.driverFor(r, st)
	if drv == nil {
		s.toIdle()
		return errNoDriver
	}

	s.mu.Lock()
	roots := make([]string, len(s.readRoots))
	copy(roots, s.readRoots)
	s.mu.Unlock()

	in := DriveInput{
		Route:     r,
		Goal:      text,
		History:   history,
		State:     st,
		Inbox:     s.Inbox,
		Out:       s.Out,
		Mode:      st.Mode, // capability captured at launch (fixed for the drive's life)
		ReadRoots: roots,   // read-only context roots captured at launch
	}
	// Attended ask seam (ask_user / set_ask_level): wired ONLY when an interactive
	// front door enabled it (askBox != nil). Captured at launch like the inbox; the
	// session-owned adapter flips Phase=AwaitingInput around the parked ask and dials
	// the per-drive budget from the conversation's ask level. Headless ⇒ nil ⇒ the
	// tools are never advertised (the structural never-block guarantee).
	if s.askBox != nil {
		in.AskUser = &askAdapter{s: s, box: s.askBox}
		// The session-backed gate approver is bound to THIS drive's ctx, so a Cancel/
		// shutdown unblocks a parked gate with a deny (fail-closed). Wired alongside the
		// ask box (attended); the chat REPL closure uses it instead of ConsoleApprover.
		in.Gate = s.NewGateApprover(ctx)
	}

	// Claim Working and launch. The drive goroutine is the single owner of the
	// drive; on completion it folds State and returns to Idle under s.mu, never
	// racing a Turn caller.
	s.mu.Lock()
	s.Phase = Working
	s.State.Active = r
	s.mu.Unlock()

	s.Log.Append(eventlog.Event{
		Task:    s.ID,
		Kind:    "session_drive_start",
		Detail:  map[string]any{"route": r.String(), "mode": st.Mode.String()},
		Backend: r.String(),
	})

	// drives was already incremented at the Routing flip (see Turn); drive()'s
	// deferred Done() balances it on the launched path.
	go s.drive(ctx, drv, in)
	return nil
}

// drive runs one driver to completion and folds the terminal result back into
// State, returning Phase to Idle. It is the only writer of State.Summary/Branch/
// LastOutcome and it takes s.mu for the fold, so a Turn that arrives the instant
// the drive ends either lands before the fold (and is pushed to the Inbox while
// still Working) or after it (and routes a fresh drive) — never a torn State.
func (s *Session) drive(ctx context.Context, drv Driver, in DriveInput) {
	defer s.drives.Done()

	res, err := drv.Drive(ctx, in)

	// A self-SUSPEND (the agent set a wake timer) is neither a completion nor a fault:
	// return to Idle and persist the bounded state (so the wake resumes from it later),
	// but DON'T fold an outcome and DON'T fire Notify — the agent is napping, not done,
	// and the re-engage happens on wake, not now.
	if errors.Is(err, backend.ErrSuspended) {
		s.mu.Lock()
		s.Phase = Idle
		s.clearDriveCancelLocked()
		folded := s.State
		s.mu.Unlock()
		s.persist(ctx, folded)
		s.Log.Append(eventlog.Event{Task: s.ID, Kind: "session_suspended", Backend: in.Route.String()})
		return
	}

	s.mu.Lock()
	if err == nil {
		s.State.Summary = res.Summary
		if res.Branch != "" {
			s.State.Branch = res.Branch
		}
		s.State.LastOutcome = res.Outcome
		// Fold a chat reply back into History so a follow-up chat turn sees the
		// agent's own prior answer ("continue, not restart"). Work drives carry their
		// continuation in the bounded Summary instead, and append nothing here. The
		// user turn that prompted this reply is already in History (appended by Turn),
		// so the assistant turn lands right after it. Growth is bounded by maybeCompact
		// at the next Turn.
		if in.Route == RouteChat && res.Outcome != "" {
			s.History = append(s.History, assistantTurn(res.Outcome))
		}
	}
	s.Phase = Idle
	s.clearDriveCancelLocked()
	folded := s.State
	s.mu.Unlock()

	// Persist the updated bounded state best-effort on each terminal drive, so a
	// follow-up after a restart continues from here rather than restarting. A nil
	// Store is a no-op; a persistence fault is logged and swallowed (durability is
	// a backstop, not a rail — the verifier and event log remain the authorities).
	s.persist(ctx, folded)

	detail := map[string]any{
		"route":    in.Route.String(),
		"verified": err == nil && res.Verified,
	}
	if err != nil {
		detail["error"] = true
	}
	s.Log.Append(eventlog.Event{
		Task:    s.ID,
		Kind:    "session_drive_done",
		Detail:  detail,
		Backend: in.Route.String(),
	})
	s.Log.Append(eventlog.Event{
		Task:   s.ID,
		Kind:   "session_fold",
		Detail: map[string]any{"verified": err == nil && res.Verified},
	})

	// Notify the (possibly detached) principal of the terminal outcome of a WORK
	// drive — the "tell me when it's done / stopped / errored" push. A plain chat
	// reply is streamed live and needs no terminal push, so it is skipped. nil Notify
	// ⇒ no-op (attached terminal/chat). The wired closure owns its own ctx/timeout.
	//
	// Fired on its own goroutine so a slow channel push (a wedged Telegram/Slack send,
	// bounded only by the closure's own timeout) does NOT delay drive()'s deferred
	// Done(): Cancel/Wait/drainShutdown block on s.drives, and conversation teardown
	// must not be coupled to an external HTTP push the design intended to be fully
	// non-blocking. The terminal State fold is already committed above, so the snapshot
	// the push reads is stable.
	if s.Notify != nil && in.Route != RouteChat {
		notify := s.Notify
		n := Notification{
			Verified: err == nil && res.Verified,
			Failed:   err != nil,
			Summary:  res.Summary.String(),
			Branch:   res.Branch,
		}
		go notify(n)
	}
}

// Notification is the terminal outcome of a WORK drive, pushed to a detached
// principal via Session.Notify. Verified is the verifier's verdict (done + green);
// Failed means the drive errored; neither set means it stopped without converging
// (likely needs attention). Summary/Branch carry the bounded result for the message.
type Notification struct {
	Verified bool
	Failed   bool
	Summary  string
	Branch   string
}

// driverFor resolves a Route to an injected Driver. RouteContinue re-enters the
// driver named by the active route carried in WorkState (continue, not restart);
// the rest map directly. A route with no wired driver yields nil (a logged
// routing failure, never a panic).
func (s *Session) driverFor(r Route, st WorkState) Driver {
	if r == RouteContinue {
		r = st.Active
	}
	switch r {
	case RouteNative:
		return s.Drivers.Native
	case RouteSupervise:
		return s.Drivers.Supervise
	case RouteProject:
		return s.Drivers.Project
	case RouteChat:
		return s.Drivers.Chat
	default:
		return nil
	}
}

// toIdle returns the Session to Idle under s.mu after a routing failure, so a
// failed route never wedges the conversation in Routing. It is reached ONLY on the
// non-launching exits (errNoRouter, a Router error, errNoDriver) — exactly the
// paths where drive() never runs — so it owns the matching drives.Done() for the
// Add(1) taken at the Routing flip. (The launched path's Done() is drive()'s defer;
// the two exits are mutually exclusive, so the counter balances either way.)
func (s *Session) toIdle() {
	s.mu.Lock()
	s.Phase = Idle
	s.clearDriveCancelLocked()
	s.mu.Unlock()
	s.drives.Done()
}

// snapshotHistory returns a copy of History so a launched drive owns its seed and
// a later Turn appending to History cannot race the drive's read of it. Callers
// hold s.mu.
func (s *Session) snapshotHistory() []model.Message {
	out := make([]model.Message, len(s.History))
	copy(out, s.History)
	return out
}

// Wait blocks until the in-flight drive goroutine (if any) has returned. It is a
// test/shutdown helper so a caller can join the drive without polling Phase.
func (s *Session) Wait() {
	s.drives.Wait()
}

// PhaseNow reads the current Phase under s.mu. A read accessor so callers (and
// tests) never touch the guarded field directly.
func (s *Session) PhaseNow() Phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Phase
}

// ActiveRoute reads the route that currently (or last) owned the work under s.mu.
// The front doors use it to flavour the thinking spinner's verbs by what the agent
// is doing (a native code change vs a whole-project build vs a chat reply). It is
// the launch-resolved route, so reading it once a drive is Working is accurate.
func (s *Session) ActiveRoute() Route {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.State.Active
}

// SetMode pins the conversation's behavioral mode (auto/discuss/plan/execute). It
// is called ONLY by a principal control verb at the front door — never from Turn
// text, an inbox follow-up, or tool output — so untrusted content can never flip
// the mode (I7); the front door's verb parser is the single authority. A change
// while a drive is Working affects only the NEXT drive: the running drive captured
// its mode in DriveInput at launch, so capability is fixed for a drive's lifetime
// (a write-capable run is never silently downgraded mid-flight, nor vice versa).
// The mode lives on WorkState, so it is persisted and survives a restart.
func (s *Session) SetMode(m Mode) {
	s.mu.Lock()
	prev := s.State.Mode
	s.State.Mode = m
	working := s.Phase != Idle
	s.mu.Unlock()
	s.Log.Append(eventlog.Event{
		Task:   s.ID,
		Kind:   "session_mode",
		Detail: map[string]any{"mode": m.String(), "prev": prev.String(), "working": working},
	})
}

// CurrentMode reads the pinned mode under s.mu. The front door uses it to ack the
// active mode and to tell the user when a switch applies only to the next turn.
func (s *Session) CurrentMode() Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.State.Mode
}

// RepoDir returns the conversation's working repository path (the absolute, already
// symlink-resolvable `-dir`). The front door's /save verb resolves its target
// against it, so a saved plan lands where the agent actually works — consistent with
// read roots, steering, and the drive worktrees — rather than against the process
// cwd. It is immutable after New, so no lock is needed.
func (s *Session) RepoDir() string { return s.Repo }

// LastAnswer returns the verbatim text of the agent's last terminal answer/plan
// (WorkState.LastOutcome) under s.mu — for a Discuss/Plan drive that is the finish
// summary (the plan/recap), for a chat reply it is the reply text. The front door's
// principal-initiated /save verb persists it to a file. The model is NEVER handed a
// write tool to do this: the human directs the write at the front door, so the
// read-only modes' structural no-write guarantee is untouched (I7). Empty before any
// drive has completed.
func (s *Session) LastAnswer() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.State.LastOutcome
}

// KeptBranch reads the branch carried in WorkState under s.mu — for a verified
// execute drive that is the KEPT branch holding the drive's verified work (the
// delivery loop: /diff previews it, /apply lands it); for a project/supervise
// drive mid-flight it is the integration tip. Empty when no drive has kept one.
func (s *Session) KeptBranch() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.State.Branch
}

// ClearKeptBranch drops the carried branch after the front door has LANDED it
// (/apply merged it into the base branch), and persists the updated bounded state
// so a restart does not resurrect an already-merged branch. Like SetMode it is
// invoked only by a principal control verb at the front door — never from Turn
// text, an inbox follow-up, or tool output (I7).
func (s *Session) ClearKeptBranch(ctx context.Context) {
	s.mu.Lock()
	prev := s.State.Branch
	s.State.Branch = ""
	folded := s.State
	s.mu.Unlock()
	if prev == "" {
		return // nothing carried; no state change, no event
	}
	s.persist(ctx, folded)
	s.Log.Append(eventlog.Event{Task: s.ID, Kind: "session_branch_clear",
		Detail: map[string]any{"had_branch": true}})
}

// AddReadRoot registers an additional READ-ONLY context root (an absolute,
// already-symlink-resolved host path — the caller validates it before calling, so
// the session stays a pure state container with no filesystem dependency). It is
// idempotent (a duplicate is ignored) and applies to the NEXT drive launched; the
// running drive captured its roots at launch. Like SetMode, it is invoked only by a
// principal control verb at the front door, never from Turn/inbox/tool text (I7).
func (s *Session) AddReadRoot(resolvedPath string) {
	s.mu.Lock()
	for _, r := range s.readRoots {
		if r == resolvedPath {
			s.mu.Unlock()
			return
		}
	}
	s.readRoots = append(s.readRoots, resolvedPath)
	n := len(s.readRoots)
	s.mu.Unlock()
	s.Log.Append(eventlog.Event{Task: s.ID, Kind: "context_add", Detail: map[string]any{"kind": "root", "count": n}})
}

// ReadRootsNow returns a copy of the registered read roots under s.mu (the front
// door uses it to show what context is attached).
func (s *Session) ReadRootsNow() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.readRoots))
	copy(out, s.readRoots)
	return out
}

// Clear resets the conversation's in-memory context — the turn History and the
// bounded WorkState summary/outcome — so the next turn starts fresh (the `/clear`
// command, and the manual sibling of auto-compaction). It deliberately PRESERVES
// the pinned Mode and the attached read roots: clearing context is not a change of
// safety posture or of what's attached. It REFUSES while a drive is in flight — a
// drive was seeded from the old History, so clearing under it would desync — and
// records a metadata-only session_clear audit event (the in-memory seed is reset;
// the append-only event log is never mutated, I5). Returns an error if not Idle.
func (s *Session) Clear() error {
	s.mu.Lock()
	if s.Phase != Idle {
		s.mu.Unlock()
		return errors.New("session: cannot clear while a run is in flight (cancel it first)")
	}
	s.History = nil
	s.State.Summary = summarize.ContextSummary{}
	s.State.LastOutcome = ""
	s.State.Branch = ""
	s.mu.Unlock()
	s.Log.Append(eventlog.Event{Task: s.ID, Kind: "session_clear"})
	return nil
}

// classifyInterrupt is the local queue-vs-steer rule for an in-flight message —
// a pure default-QUEUE function with NO model round-trip. Immediacy is the whole
// point of steering, so it is reserved for an explicit prefix mark: a leading
// '!' or a '/steer' command. Everything else queues, folded as an ordinary user
// turn at the next loop boundary.
func classifyInterrupt(text string) inbox.Mode {
	t := strings.TrimLeft(text, " \t")
	if strings.HasPrefix(t, "!") {
		return inbox.Steer
	}
	if t == "/steer" || strings.HasPrefix(t, "/steer ") {
		return inbox.Steer
	}
	return inbox.Queue
}

// userTurn builds the canonical one-block user message the loop folds in — the
// same shape native.go:188 and super.go:191 build. The principal's text is a
// trusted instruction (an un-guard.Wrap'd user turn); fencing applies only to
// tool/file/peer data the loop reads back, never to a principal message.
func userTurn(text string) model.Message {
	return model.Message{
		Role:    "user",
		Content: []model.Block{{Type: "text", Text: text}},
	}
}

// assistantTurn builds the canonical one-block assistant message folded into History
// after a chat reply, so a follow-up chat turn continues with the agent's own prior
// answer in context rather than re-entering with only the user's questions.
func assistantTurn(text string) model.Message {
	return model.Message{
		Role:    "assistant",
		Content: []model.Block{{Type: "text", Text: text}},
	}
}
