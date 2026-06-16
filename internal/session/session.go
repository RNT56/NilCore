package session

import (
	"context"
	"errors"
	"strings"
	"sync"

	"nilcore/internal/budget"
	"nilcore/internal/emit"
	"nilcore/internal/eventlog"
	"nilcore/internal/inbox"
	"nilcore/internal/memory"
	"nilcore/internal/model"
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

	// Sizer is the native-vs-supervise sizing heuristic used ONLY when the user has
	// pinned ModeExecute (which bypasses the auto-router): a complex goal routes to
	// the supervisor, otherwise the single native loop. It is the SAME pure function
	// the SupervisorFirstRouter holds (chatShouldSupervise), injected here so the
	// session need not depend on the concrete router type. nil ⇒ "not complex" (the
	// conservative single-loop default). Unused in ModeAuto (the router sizes there).
	Sizer func(goal string) bool

	mu      sync.Mutex      // guards Phase + History + driveCancel
	Phase   Phase           // current conversational state
	History []model.Message // canonical turns — the shape native/super build
	State   WorkState       // bounded carry-over (never raw transcripts)
	drives  sync.WaitGroup  // tracks the in-flight drive goroutine (test sync)

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
		ID:     id,
		Sender: sender,
		Repo:   repo,
		Log:    log,
		Inbox:  inbox.New(log, id),
		Phase:  Idle,
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
		// In-flight: fold the follow-up into History and the running loop's Inbox.
		mode := classifyInterrupt(text)
		s.History = append(s.History, userMsg)
		phase := s.Phase
		s.mu.Unlock()

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

	drv := s.driverFor(r, st)
	s.Log.Append(eventlog.Event{
		Task: s.ID,
		Kind: "session_route",
		Detail: map[string]any{
			"route":    r.String(),
			"len_text": len(text),
		},
	})
	if drv == nil {
		s.toIdle()
		return errNoDriver
	}

	in := DriveInput{
		Route:   r,
		Goal:    text,
		History: history,
		State:   st,
		Inbox:   s.Inbox,
		Out:     s.Out,
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
		Detail:  map[string]any{"route": r.String()},
		Backend: r.String(),
	})

	s.drives.Add(1)
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

	s.mu.Lock()
	if err == nil {
		s.State.Summary = res.Summary
		if res.Branch != "" {
			s.State.Branch = res.Branch
		}
		s.State.LastOutcome = res.Outcome
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
// failed route never wedges the conversation in Routing.
func (s *Session) toIdle() {
	s.mu.Lock()
	s.Phase = Idle
	s.clearDriveCancelLocked()
	s.mu.Unlock()
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
