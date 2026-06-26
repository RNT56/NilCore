package session

// drivers.go implements the concrete Drivers (C2-T03): the thin glue that maps
// each Route onto EXISTING NilCore machinery with NO new agentic logic. A Driver
// takes a DriveInput (goal, History-to-continue, bounded State, the live user
// Inbox, the reasoning Emitter) and returns a DriveResult the Session folds back
// into WorkState. Each driver's job is exactly two things:
//
//  1. invoke the right machine with the session's Inbox + Emitter WIRED IN, so a
//     mid-work steer/queue reaches the running loop and live reasoning is surfaced;
//  2. fold the machine's terminal outcome into the bounded WorkState (via
//     summarize) — never a raw transcript.
//
// The machines themselves (agent.Orchestrator.executeSingle → backend.Native,
// super.Supervisor.Run, project.Loop.Run) are heavy: they need a worktree, a
// sandbox, a verifier factory, a roster, a bus, an integrator — all constructed at
// the wiring site (cmd/nilcore/build.go) from deps this leaf package does not own.
// So, exactly like project.Loop's Plan/RunSlice/Verifier seams and
// super.Supervisor's Spawn/Code/Integrate seams, the concrete drivers here are
// FUNC-SEAM ADAPTERS: the wiring site supplies a run closure that constructs and
// runs the real machine (with the per-drive Inbox/Seed/Emitter injected); the
// driver owns only the Route→machine selection and the summarize fold-back. This
// keeps session a clean leaf — it imports no agent/super/project/backend/worktree/
// sandbox machinery and so can never form an import cycle with them — while still
// mapping every Route onto the real loops with no logic of its own.
//
// Trust line (I7): the principal's History/Goal is trusted user text the machine
// folds as ordinary user turns; the drivers add nothing executable and read no
// tool/file/peer output, so there is no fencing concern at this seam (the loops
// own their own guard.Wrap fencing). Budget (§6): a driver NEVER keys spend by its
// per-drive task id — the conversation-metered provider the wiring closure runs
// the machine with carries the conversation budget key, so N back-to-back drives
// share ONE ceiling, never N×ceiling.

import (
	"context"
	"fmt"
	"sync/atomic"

	"nilcore/internal/emit"
	"nilcore/internal/model"
	"nilcore/internal/summarize"
)

// DriveOutcome is the machine-shaped terminal result a run closure reports — the
// lowest-common-denominator over agent.Outcome / super.Outcome / project.Outcome,
// so the wiring closures map their machine's native outcome onto it with a thin
// field copy and the drivers fold it back uniformly. Summary is the machine's own
// account (bounded text); Branch is the integration tip when a project/supervisor
// drive converged on one (empty otherwise); Verified is the VERIFIER's verdict
// (the sole authority on done, I2) — never a model self-claim. It carries no
// transcript: the fold-back distils it into the bounded WorkState.
type DriveOutcome struct {
	Summary  string // the machine's bounded account of what it did
	Branch   string // integration tip (project/supervise) — empty when none
	Verified bool   // the verifier signed off (I2) — never a self-report
}

// RunNativeFunc runs ONE native drive: it builds a fresh worktree, constructs a
// backend.Native with the session's Inbox + Emitter + Seed:=History WIRED IN, runs
// the orchestrator's single-task path (agent.Orchestrator.executeSingle), and
// re-verifies as the gate. The wiring site (C3) supplies it; it is where the real
// backend/orchestrator/worktree machinery lives. The closure MUST run the loop
// with the conversation-metered provider (so spend keys by the conversation id,
// not the per-drive task id — §6) and pass in.Inbox/in.Emitter/in.Seed unchanged.
type RunNativeFunc func(ctx context.Context, in NativeRun) (DriveOutcome, error)

// NativeRun is what a native run closure receives. TaskID is the per-drive id for
// the worktree + eventlog ONLY (never the budget key, §6). Goal is the principal's
// instruction; Seed is the prior History the loop continues on top of (the
// persistence requirement — continue, not restart); Inbox/Emitter are the session
// seams the closure wires onto backend.Native so a mid-work steer/queue reaches
// the loop and reasoning is surfaced.
type NativeRun struct {
	TaskID  string          // worktree/eventlog key — NEVER the budget key
	Goal    string          // the principal's instruction for this drive
	Seed    []model.Message // prior History the loop continues on (nil = fresh)
	Inbox   InboxHandle     // the live user→agent seam wired into backend.Native
	Emitter emit.Emitter    // the reasoning sink wired into backend.Native
	// Mode is the capability the closure builds the backend with (captured at drive
	// launch). ReadOnly modes ⇒ write-free registry + DisableShell + pass-through
	// verifier; Execute/Auto ⇒ the full write set gated by the real verifier (I2).
	Mode Mode
	// ReadRoots are the read-only context roots the closure wires onto the read/
	// search tools (absolute, resolved). Empty ⇒ worktree-only (byte-identical).
	ReadRoots []string
	// AskUser is the attended human-clarification seam wired onto backend.Native
	// (ask_user / set_ask_level). nil for every headless drive ⇒ the tools are never
	// advertised (the structural never-block guarantee, I3/I4).
	AskUser AskerHandle
}

// RunSuperviseFunc runs one supervised drive: it constructs/uses a
// super.Supervisor with the session's Inbox + Out WIRED IN and calls Run(ctx,
// goal). The user Inbox is the second concurrent source the supervisor folds at
// the round boundary beside its subagent findings. The wiring site supplies it.
type RunSuperviseFunc func(ctx context.Context, goal string, seed []model.Message, in InboxHandle, out emit.Emitter) (DriveOutcome, error)

// RunProjectFunc runs one whole-project drive: project.Loop.Run(ctx), seeding the
// loop's initial ContextSummary from the carried WorkState so a follow-up
// continues the project rather than restarting it. The project loop has no inbox
// seam of its own (its agentic work happens inside the supervisor it drives, which
// carries the Inbox); the Emitter surfaces the loop's progress. The wiring site
// supplies it.
type RunProjectFunc func(ctx context.Context, goal string, seed summarize.ContextSummary, out emit.Emitter) (DriveOutcome, error)

// nativeDriver maps RouteNative onto the orchestrator's single-task path. It holds
// only the run closure + the metered chat/summarize provider used to distil the
// terminal outcome into bounded WorkState. No worktree/sandbox/backend lives here —
// the closure owns that — so the driver is pure glue.
type nativeDriver struct {
	run RunNativeFunc
	sum model.Provider // conversation-metered provider for the summarize fold-back; nil-safe
	id  string         // conversation id — the worktree/eventlog seq prefix base
	seq int64          // monotonic per-drive sequence for the worktree task id (atomic)
}

// NewNativeDriver builds the RouteNative driver. run is the wiring closure that
// runs backend.Native (with Inbox/Seed/Emitter injected) under the orchestrator's
// single-task path; sum is the conversation-metered provider the fold-back uses to
// summarize the outcome into bounded WorkState (nil falls back to a no-model
// minimal summary); id is the conversation id (the per-drive worktree task id is
// id + "-" + seq, fine for the worktree/eventlog but NEVER the budget key — §6).
func NewNativeDriver(run RunNativeFunc, sum model.Provider, id string) Driver {
	return &nativeDriver{run: run, sum: sum, id: id}
}

// Drive runs one native drive: invoke the wiring closure with the session's
// Inbox/Emitter/Seed:=History wired in, then fold the verifier-judged outcome into
// a DriveResult. A nil run closure yields a structured error (the driver is unwired)
// rather than a panic, so the Session returns cleanly to Idle.
func (d *nativeDriver) Drive(ctx context.Context, in DriveInput) (DriveResult, error) {
	if d.run == nil {
		return DriveResult{}, fmt.Errorf("session: native driver has no run closure")
	}
	out, err := d.run(ctx, NativeRun{
		TaskID:    fmt.Sprintf("%s-%d", d.id, atomic.AddInt64(&d.seq, 1)),
		Goal:      in.Goal,
		Seed:      nativeSeed(in), // History, or the restored Summary after a restart
		Inbox:     in.Inbox,
		Emitter:   in.Out,
		Mode:      in.Mode,      // capability captured at launch (read-only vs full)
		ReadRoots: in.ReadRoots, // read-only context roots captured at launch
		AskUser:   in.AskUser,   // attended ask seam (nil for headless / supervised)
	})
	if err != nil {
		return DriveResult{}, fmt.Errorf("native drive: %w", err)
	}
	return foldOutcome(ctx, d.sum, in.Goal, out), nil
}

// resumeSeedPreamble frames a restored bounded summary that is seeded into a native
// drive after a process restart, so the model reads it as prior-session context to
// continue from (a principal-trusted handoff, never untrusted data).
const resumeSeedPreamble = "[resumed session — prior context; continue from here]\n\n"

// nativeSeed picks the message seed for a native drive. Normally it is the
// accumulated turn History (continue, not restart). But History is never persisted —
// only the bounded WorkState is (persist.go) — so after a process RESTART the
// History is gone and a continuation would re-enter with just the new goal turn and
// silently RESTART, contradicting the "↻ resumed the previous conversation" promise.
// When the History carries nothing but the current turn yet a bounded Summary was
// restored, prepend that Summary as one context turn so "continue" actually
// continues. In normal in-process operation History has grown past one turn, so this
// never fires and the seed is byte-identical to before. (Only the native path needs
// this; projectDriver already passes State.Summary to its loop directly.)
func nativeSeed(in DriveInput) []model.Message {
	if len(in.History) <= 1 && in.State.Summary.Goal != "" {
		seed := make([]model.Message, 0, len(in.History)+1)
		seed = append(seed, userTurn(resumeSeedPreamble+in.State.Summary.String()))
		seed = append(seed, in.History...)
		return seed
	}
	return in.History
}

// superviseDriver maps RouteSupervise onto super.Supervisor.Run.
type superviseDriver struct {
	run RunSuperviseFunc
	sum model.Provider
}

// NewSuperviseDriver builds the RouteSupervise driver over the wiring closure that
// runs super.Supervisor.Run with the session's Inbox + Out wired in; sum is the
// conversation-metered provider for the fold-back (nil-safe).
func NewSuperviseDriver(run RunSuperviseFunc, sum model.Provider) Driver {
	return &superviseDriver{run: run, sum: sum}
}

// Drive runs one supervised drive: invoke the wiring closure with the session's
// Inbox + Emitter wired in (the inbox is the supervisor's second concurrent
// source, folded beside subagent findings), then fold the verifier-judged outcome.
func (d *superviseDriver) Drive(ctx context.Context, in DriveInput) (DriveResult, error) {
	if d.run == nil {
		return DriveResult{}, fmt.Errorf("session: supervise driver has no run closure")
	}
	out, err := d.run(ctx, in.Goal, in.History, in.Inbox, in.Out)
	if err != nil {
		return DriveResult{}, fmt.Errorf("supervise drive: %w", err)
	}
	return foldOutcome(ctx, d.sum, in.Goal, out), nil
}

// projectDriver maps RouteProject onto project.Loop.Run, seeding the loop's
// initial ContextSummary from the carried WorkState (continue, not restart).
type projectDriver struct {
	run RunProjectFunc
	sum model.Provider
}

// NewProjectDriver builds the RouteProject driver over the wiring closure that runs
// project.Loop.Run, seeding State.Summary into the loop's initial ContextSummary;
// sum is the conversation-metered provider for the fold-back (nil-safe).
func NewProjectDriver(run RunProjectFunc, sum model.Provider) Driver {
	return &projectDriver{run: run, sum: sum}
}

// Drive runs one whole-project drive: invoke the wiring closure seeded with the
// carried WorkState summary (so a follow-up continues the project) and the
// Emitter, then fold the verifier-judged outcome.
func (d *projectDriver) Drive(ctx context.Context, in DriveInput) (DriveResult, error) {
	if d.run == nil {
		return DriveResult{}, fmt.Errorf("session: project driver has no run closure")
	}
	out, err := d.run(ctx, in.Goal, in.State.Summary, in.Out)
	if err != nil {
		return DriveResult{}, fmt.Errorf("project drive: %w", err)
	}
	return foldOutcome(ctx, d.sum, in.Goal, out), nil
}

// chatDriver maps RouteChat onto a single metered Complete over History — NO loop,
// NO worktree, NO verifier. It answers meta/conversational questions ("what are you
// working on?") cheaply. The reply is surfaced through the Emitter so the user sees
// it on the same surface as the loop's reasoning. Because it does no work, there is
// nothing to verify: DriveResult.Verified is true vacuously (no failed gate), and
// it folds no Branch and a bounded summary that carries the goal forward unchanged.
type chatDriver struct {
	model model.Provider // the conversation-metered provider (chat reply + ledger)
}

// NewChatDriver builds the RouteChat driver over the conversation-metered provider.
// A nil provider yields a driver that returns a structured error rather than a
// panic (chat is unavailable without a model).
func NewChatDriver(m model.Provider) Driver {
	return &chatDriver{model: m}
}

const chatSys = `You are the conversational front door of a coding agent. Answer the user's question about the work directly and briefly using the conversation so far. You are NOT taking any action or writing any code here — just reply in plain language. If the user is asking you to do coding work, say so and that they should restate it as an instruction.`

// Drive answers one chat turn: one metered Complete over the History (the goal is
// already the last user turn in History when the Session routed here, but it is
// appended defensively in case a caller passes an empty History), surfaced through
// the Emitter. It runs zero loops and touches no worktree. The metered provider
// charges the conversation ledger, so even a chat reply counts against the §6
// ceiling. A model transport error is returned (the Session returns to Idle); a
// reply is never treated as a verifier verdict (there is nothing to verify).
func (d *chatDriver) Drive(ctx context.Context, in DriveInput) (DriveResult, error) {
	if d.model == nil {
		return DriveResult{}, fmt.Errorf("session: chat driver has no model")
	}
	msgs := chatMessages(in.History, in.Goal)
	resp, err := d.model.Complete(ctx, chatSys, msgs, nil, 1024)
	if err != nil {
		return DriveResult{}, fmt.Errorf("chat drive: %w", err)
	}
	reply := firstTextBlock(resp.Content)
	if in.Out != nil && reply != "" {
		in.Out.Emit(emit.Event{Kind: emit.KindIntent, Text: reply})
	}
	// Chat does no work: carry the prior goal forward unchanged (so a later
	// RouteContinue still references it) and record the reply as the data-only
	// outcome tail. Verified is true vacuously — there was no gate to fail.
	return DriveResult{
		Summary:  in.State.Summary,
		Outcome:  reply,
		Verified: true,
	}, nil
}

// chatMessages builds the message slice for a chat reply: the prior History
// verbatim, plus the goal as a trailing user turn when History is empty (so a
// directly-driven chat with no prior turns still has something to answer). When
// History already ends with the goal turn (the normal Session path), it is used
// as-is — chat reads History as the principal's trusted conversation, never
// fencing it (I7).
func chatMessages(history []model.Message, goal string) []model.Message {
	if len(history) > 0 {
		out := make([]model.Message, len(history))
		copy(out, history)
		return out
	}
	return []model.Message{userTurn(goal)}
}

// foldOutcome distils a machine's terminal DriveOutcome into the bounded
// DriveResult the Session folds into WorkState. It NEVER carries a transcript: the
// machine's bounded Summary text is summarized into a ContextSummary (reusing
// summarize's discipline — goal/constraints/decisions/remaining), so a follow-up
// re-enters seeded with intent, not replayed history. A nil provider (or a
// summarize failure) degrades to a minimal summary that still carries the goal and
// a bounded tail, so the fold never fails outright. Verified and Branch pass
// through verbatim — the verifier's verdict (I2) and the integration tip are facts,
// not summaries.
func foldOutcome(ctx context.Context, m model.Provider, goal string, out DriveOutcome) DriveResult {
	summary := summarize.ContextSummary{Goal: goal, Remaining: out.Summary}
	if m != nil {
		if cs, err := summarize.Summarize(ctx, m, goal, out.Summary); err == nil {
			summary = cs
		}
	}
	return DriveResult{
		Summary:  summary,
		Branch:   out.Branch,
		Outcome:  out.Summary,
		Verified: out.Verified,
	}
}
