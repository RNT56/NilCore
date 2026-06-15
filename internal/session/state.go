// Package session is the persistent conversational state container for the
// conversational front door (C2-T01). One Session holds a single conversation:
// its turn history, its bounded work-state carry-over, the user→agent inbox the
// running loop drains, and a phase machine that makes the Idle→Working→Idle spine
// re-enterable so a follow-up continues the work rather than restarting it.
//
// This file holds the value types — Phase, Route, WorkState — that the Session
// carries. The Session itself, its single Turn entry point, and the local
// queue-vs-steer rule live in session.go. The Router and Driver seams are
// injected interfaces (declared here, satisfied by C2-T02/C2-T03) so this core
// compiles and is testable with fakes before the routing/driving machinery
// exists. It is a stdlib-only leaf (sync) plus internal/{model,inbox,emit,
// eventlog,summarize,memory,budget}; it imports no loop or channel machinery.
package session

import (
	"context"

	"nilcore/internal/emit"
	"nilcore/internal/model"
	"nilcore/internal/summarize"
)

// Phase is the Session's conversational state. The Idle→Routing→Working→
// Terminal→Idle spine is exactly today's orchestrator/loop flow, wrapped to be
// re-enterable; AwaitingGate is the parked state while a drive blocks on the
// human approver. Only the Working→Inbox edges (queue/steer) are new.
type Phase int

const (
	// Idle: no drive is running; the next Turn routes and launches one.
	Idle Phase = iota
	// Routing: a Turn has been accepted and the router is choosing a driver. A
	// transient state held only across the synchronous route+launch handoff.
	Routing
	// Working: a drive goroutine owns the work; a Turn pushes to the inbox
	// (queued or steered) instead of launching.
	Working
	// Terminal: the drive has finished and its result is being folded into the
	// work-state; a transient state on the way back to Idle.
	Terminal
	// AwaitingGate: a drive is blocked on the human approver (an irreversible
	// action). A Turn still pushes to the inbox; the gate answer resumes Working.
	AwaitingGate
)

// String renders the phase for the metadata-only audit events (never a body).
func (p Phase) String() string {
	switch p {
	case Idle:
		return "idle"
	case Routing:
		return "routing"
	case Working:
		return "working"
	case Terminal:
		return "terminal"
	case AwaitingGate:
		return "awaiting_gate"
	default:
		return "unknown"
	}
}

// Route names which machine owns a drive. RouteContinue re-enters the driver
// named by WorkState.Active (the "continue, not restart" path); the rest map to
// the native loop, the supervisor, the project loop, or a no-loop chat reply.
// The router (C2-T02) decides the Route; the drivers (C2-T03) execute it. The
// enum lives here because WorkState.Active carries it across drives.
type Route int

const (
	// RouteContinue re-enters the driver named by WorkState.Active.
	RouteContinue Route = iota
	// RouteNative runs the single native loop (orchestrator's single-task path).
	RouteNative
	// RouteSupervise runs the multi-agent supervisor.
	RouteSupervise
	// RouteProject runs the whole-project loop.
	RouteProject
	// RouteChat answers without any loop — one metered completion over History.
	RouteChat
)

// String renders the route for audit events.
func (r Route) String() string {
	switch r {
	case RouteContinue:
		return "continue"
	case RouteNative:
		return "native"
	case RouteSupervise:
		return "supervise"
	case RouteProject:
		return "project"
	case RouteChat:
		return "chat"
	default:
		return "unknown"
	}
}

// WorkState is the bounded carry-over between drives — never a raw transcript.
// It reuses summarize.ContextSummary's discipline (goal/constraints/decisions/
// remaining) so a follow-up re-enters a driver seeded with intent, not a full
// replayed history. Active names which driver currently or last owned the work
// (so RouteContinue knows where to re-enter); Branch is the integration tip when
// a project/supervisor drive is mid-flight; LastOutcome is the data-only tail of
// the last terminal result.
type WorkState struct {
	Summary     summarize.ContextSummary // bounded handoff (no raw transcripts)
	Active      Route                    // driver that currently/last owned the work
	Branch      string                   // integration tip when project/super mid-flight
	LastOutcome string                   // data-only tail of the last terminal result
}

// Router chooses which machine runs a new (non-in-flight) drive. It is injected
// so C2-T01 compiles and tests with a fake before the metered classifier
// (SupervisorFirstRouter, C2-T02) lands. text is the principal's trusted message
// and st is the current work-state (for RouteContinue detection).
type Router interface {
	Route(ctx context.Context, text string, st WorkState) (Route, error)
}

// DriveInput is what a Session hands a driver to run one drive: the goal text,
// the conversation History to continue from (not restart), the bounded work
// State, the user inbox the running loop drains, and the reasoning sink. The
// concrete drivers (C2-T03) map this onto the existing native/supervisor/project
// machinery with no new agentic logic.
type DriveInput struct {
	Route   Route
	Goal    string
	History []model.Message
	State   WorkState
	Inbox   InboxHandle
	Out     emit.Emitter
}

// DriveResult is a driver's terminal outcome, folded into WorkState on
// completion. Summary is the bounded carry-over for the next drive; Branch is
// the integration tip (if any); Outcome is the data-only tail; Verified records
// that the verifier (the sole authority on "done", I2) signed off.
type DriveResult struct {
	Summary  summarize.ContextSummary
	Branch   string
	Outcome  string
	Verified bool
}

// Driver runs one drive to a terminal result. It is injected (the Drivers set,
// C2-T03) so this core compiles and tests with a fake. A driver must honor ctx
// cancellation (shutdown) and drain its DriveInput.Inbox at loop boundaries.
type Driver interface {
	Drive(ctx context.Context, in DriveInput) (DriveResult, error)
}

// InboxHandle is the minimal handle a driver needs onto the user-message seam:
// it drains queued turns at a boundary and selects on the steer signal. It is
// satisfied by *inbox.Box (the concrete seam the Session owns); declaring it as
// an interface here keeps the driver seam decoupled from the concrete box, the
// same discipline backend.Inbox uses.
type InboxHandle interface {
	Drain() []model.Message
	Steer() <-chan struct{}
}
