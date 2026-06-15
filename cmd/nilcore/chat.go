// chat.go wires the `nilcore chat` subcommand (docs/CONVERSATIONAL.md §6/§7): the
// PRIMARY conversational front door. From one terminal the user just talks — a
// typo fix to "plan and ship a whole service" — and the harness infers the machine
// (native loop / supervisor / project loop) and lets the user queue or steer
// messages mid-work.
//
// It is purely WIRING over the C0–C2 machinery already in internal/: one
// session.Session (ID "chat-local", the terminal user as principal) holds the
// conversation; a metered provider keyed by that conversation id charges ONE
// shared budget.Ledger (the conversation wall, §6); a SupervisorFirstRouter picks
// the machine; the three drivers (NewNativeDriver/NewSuperviseDriver/
// NewProjectDriver) plus a chat driver run the existing loops with the session's
// Inbox + Emitter injected so a mid-work steer/queue reaches the running loop and
// live reasoning streams to stdout. A stdin reader goroutine reads lines WHILE the
// agent works and feeds each to Session.Turn: a plain line QUEUEs (folded at the
// next loop boundary), a '!'/'/steer' line STEERs (the agent pauses at the next
// step, takes the feedback in, then resumes or changes course — it never discards
// in-flight work). A '/cancel' (or '/stop') aborts the current run but stays in
// the conversation; Ctrl-C cancels the conversation ctx and exits.
//
// run/build/serve/init/doctor are untouched: this is a new dispatch case plus this
// file, and the native/supervisor loops stay byte-identical with a nil Inbox/
// Emitter on every path but chat.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"nilcore/internal/advisor"
	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/budget"
	"nilcore/internal/emit"
	"nilcore/internal/eventlog"
	"nilcore/internal/model"
	"nilcore/internal/policy"
	"nilcore/internal/sandbox"
	"nilcore/internal/session"
	"nilcore/internal/summarize"
	"nilcore/internal/tools"
	"nilcore/internal/verify"
)

// chatConvoID is the fixed conversation/budget key for the local terminal
// front door. The whole interactive session is ONE conversation, so every drive
// (and the router's classifier call) charges this single key against one ceiling
// (§6) — never N×ceiling across follow-ups.
const chatConvoID = "chat-local"

// chatPrincipal is the pinned Sender for the local terminal session. The terminal
// user is the principal by construction (no channel allowlist applies — they hold
// the keyboard), so the Session records a fixed local identity.
const chatPrincipal = "local"

// chatDefaultBudget is the global dollar ceiling applied to the conversation when
// -budget is not set. It is a real wall (the metered provider charges the shared
// ledger; a breach returns budget.ErrCeiling and aborts the drive), sized for an
// interactive session rather than an unattended build.
const chatDefaultBudget = 10.0

// chatFlags are the chat subcommand's flags. It reuses registerCommon for the
// shared boot/runtime/verifier knobs (so -dir, -runtime, -image, -verify,
// -max-steps, -backend, -config, -log behave exactly as for run) and adds the
// conversation budget ceiling.
type chatFlags struct {
	common commonFlags
	budget *float64
}

// chatMain is the `nilcore chat` entry point and the bare-`nilcore` default. It
// resolves boot context, builds ONE Session wired to the full machinery, and runs
// the line-based REPL until EOF (Ctrl-D) or interrupt (Ctrl-C). The REPL never
// touches a container or a model directly — every effect flows through the
// Session's drivers, exactly as serve/build do.
func chatMain(args []string) {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	cf := chatFlags{
		common: registerCommon(fs),
		budget: fs.Float64("budget", chatDefaultBudget, "global dollar ceiling for the whole conversation (a hard wall via the meter)"),
	}
	_ = fs.Parse(args)

	b := loadBoot(*cf.common.config)
	applyConfigDefaults(cf.common, b.cfg, flagsSet(fs))

	absDir := mustAbs(*cf.common.dir)
	log := openLog(*cf.common.logPath)
	defer log.Close()

	prov, err := resolveProvider(*cf.common.backendName, b)
	if err != nil {
		fatal(err)
	}
	if prov == nil {
		// A delegated backend (codex/claude-code) has no model.Provider to meter or
		// to route/summarize/chat with; chat is a native-loop experience by design.
		fatal(fmt.Errorf("nilcore chat requires the native backend (a model provider to route and converse with); "+
			"the %q backend has no native model — use `nilcore serve` for a delegated-backend channel", *cf.common.backendName))
	}

	sess, err := buildChatSession(chatDeps{
		flags:    cf,
		provider: prov,
		boot:     b,
		log:      log,
		baseRepo: absDir,
	})
	if err != nil {
		fatal(err)
	}

	// Ctrl-C / SIGTERM cancels the whole conversation ctx, so the in-flight drive
	// unwinds to a clean interrupted result and the REPL exits. (To abort only the
	// current run and keep talking, use /cancel — see runChatCommand.)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintln(os.Stdout, chatBanner)
	if err := chatREPL(ctx, sess, os.Stdin, os.Stdout); err != nil && err != io.EOF {
		fatal(err)
	}
	// Let any in-flight drive unwind on the cancelled ctx before we exit, so its
	// worktree cleanup runs and no drive goroutine is abandoned mid-write.
	sess.Wait()
}

// chatBanner is the one-time greeting printed before the prompt. It states the two
// modes (queue default, '!' steers) and the controls so the user knows the surface
// without reading docs.
const chatBanner = `nilcore chat — talk to the agent; it picks the machine and works while you type.
  <text>            queue a message (folded in at the next step)
  !<text> /steer …  steer: pause, take your feedback in, then resume or change course
  /cancel  /stop    abort the current run (and stay in the conversation)
  /status           show what the agent is working on
  /quit  (Ctrl-D)   leave`

// chatDeps is the resolved input to buildChatSession: everything the wiring needs
// after flags + boot resolve. Keeping it a plain struct lets the hermetic test
// inject a scripted provider, a temp repo, and an in-memory log and assert the
// Session is wired correctly (router + four drivers, conversation-keyed budget)
// without a container or network.
type chatDeps struct {
	flags    chatFlags
	provider model.Provider
	boot     boot
	log      *eventlog.Log
	baseRepo string
}

// buildChatSession assembles the one conversation Session for the terminal front
// door. The load-bearing properties (§6):
//
//   - ONE shared budget.Ledger gets SetGlobalCeiling(-budget): the conversation
//     wall. EVERY provider the session uses (router classifier, the drivers'
//     loops, chat replies, the summarize fold-back) is meter-wrapped against it
//     under the SAME conversation Task key (chatConvoID), so N back-to-back drives
//     share ONE ceiling — never N×ceiling.
//   - The SupervisorFirstRouter classifies each new drive with that metered
//     provider and reconciles the proposal with the chatShouldSupervise heuristic.
//   - The three drivers run the EXISTING native/supervisor/project machinery with
//     the session's Inbox + Emitter injected (steer/queue reach the loop; reasoning
//     surfaces), and the chat driver answers meta questions with no loop.
func buildChatSession(d chatDeps) (*session.Session, error) {
	// The conversation wall: one ledger, one global ceiling.
	ledger := budget.New()
	ledger.SetGlobalCeiling(*d.flags.budget)

	// One metered provider keyed by the conversation id. Reused for routing,
	// chat replies, and the summarize fold-back so all of it charges one ceiling.
	metered := meterProvider(d.provider, ledger, chatConvoID)

	sess := session.New(chatConvoID, chatPrincipal, d.baseRepo, d.log)
	// Live reasoning/intent streams to stdout (the steer surface, §5.3).
	sess.Out = emit.NewWriter(os.Stdout)
	sess.Budget = ledger

	sess.Router = &session.SupervisorFirstRouter{
		Classifier:      metered,
		ShouldSupervise: chatShouldSupervise,
		Log:             d.log,
		ID:              chatConvoID,
	}

	sess.Drivers = session.Drivers{
		Native:    session.NewNativeDriver(chatNativeRun(d, metered), metered, chatConvoID),
		Supervise: session.NewSuperviseDriver(chatSuperviseRun(d, ledger), metered),
		Project:   session.NewProjectDriver(chatProjectRun(d, ledger), metered),
		Chat:      session.NewChatDriver(metered),
	}
	return sess, nil
}

// chatShouldSupervise is the native-vs-supervise sizing heuristic the router
// reconciles its classifier proposal against (§3.4). It is a deliberately simple,
// pure-function judgment over the goal text — long, multi-component goals warrant
// the supervisor; a short localized ask stays the single native loop. The router
// uses it both to upgrade/downgrade the classifier and as the no-model fallback on
// unparseable output, so it must never panic and must be cheap.
func chatShouldSupervise(goal string) bool {
	g := strings.ToLower(goal)
	// A genuinely large surface (many words) or an explicit multi-component verb is
	// the supervisor's domain; everything else is one native loop.
	if len(strings.Fields(goal)) >= 40 {
		return true
	}
	for _, k := range []string{"build a", "scaffold", "whole service", "microservice", "several files", "end to end", "end-to-end", "from scratch"} {
		if strings.Contains(g, k) {
			return true
		}
	}
	return false
}

// chatNativeRun returns the RunNativeFunc the native driver invokes: it runs ONE
// native drive through the orchestrator's single-task path (fresh worktree,
// backend.Native, final verify — I2 unchanged) with the session's Inbox + Seed
// (the prior History — continue, not restart) + Emitter WIRED IN. The loop runs
// with the conversation-metered provider so spend keys by the conversation id, not
// the per-drive task id (§6); the per-drive TaskID is the worktree/eventlog key
// only.
func chatNativeRun(d chatDeps, metered model.Provider) session.RunNativeFunc {
	adv := resolveAdvisor(*d.flags.common.backendName, d.boot, d.flags.common)
	return func(ctx context.Context, in session.NativeRun) (session.DriveOutcome, error) {
		// The per-worktree backend+verifier factory, with the session's Inbox/Seed/
		// Emitter spliced onto backend.Native so a mid-work steer/queue reaches the
		// loop and reasoning surfaces. Everything else is exactly the run path's
		// envFactory (sandbox over the worktree, the project verifier, the advisor).
		newEnv := func(dir string) agent.Env {
			box := sandbox.NewContainer(*d.flags.common.runtime, *d.flags.common.image, dir)
			v := verify.New(box, *d.flags.common.checkCmd)
			n := chatNativeBackend(d, metered, adv, box, v, in)
			return agent.Env{Backend: n, Verifier: v}
		}

		orch := &agent.Orchestrator{
			BaseRepo: d.baseRepo,
			NewEnv:   newEnv,
			Log:      d.log,
			Router:   agent.SingleRouter{},
			Spawner:  agent.NoSpawner{},
			Approver: policy.NewConsoleApprover(os.Stdin, os.Stdout),
		}

		out, err := orch.Execute(ctx, backend.Task{ID: in.TaskID, Goal: in.Goal})
		if err != nil {
			return session.DriveOutcome{}, err
		}
		return session.DriveOutcome{Summary: out.Summary, Verified: out.Verified}, nil
	}
}

// chatNativeBackend builds the backend.Native for one chat drive with the
// conversational seams attached: the session Inbox (steer/queue), Seed (the prior
// History — continue, not restart), and Emitter (live reasoning). It mirrors
// buildBackend's native arm but threads the three additive gates; with a nil
// Inbox/Emitter the loop would be byte-identical to a plain run (here they are
// always wired, since chat is the seam's reason to exist).
func chatNativeBackend(d chatDeps, prov model.Provider, adv advisorCfg, box sandbox.Sandbox, v verify.Verifier, in session.NativeRun) *backend.Native {
	n := &backend.Native{
		Model:        prov,
		Box:          box,
		Verifier:     v,
		Log:          d.log,
		Tools:        tools.Default(),
		CommandGuard: policy.DefaultCommandPolicy().Check,
		MaxSteps:     *d.flags.common.maxSteps,
		Seed:         in.Seed,
	}
	if in.Inbox != nil {
		n.Inbox = in.Inbox
	}
	if in.Emitter != nil {
		n.Emitter = in.Emitter
	}
	if adv.prov != nil {
		// A fresh advisor per drive so its per-drive consult ceiling is honored,
		// exactly as the run path's buildBackend does.
		n.Advisor = advisor.New(adv.prov, adv.maxCalls)
		n.EscalateAfter = adv.escalateAfter
	}
	return n
}

// chatSuperviseRun returns the RunSuperviseFunc the supervise driver invokes: it
// assembles the full multi-agent stack (via buildStack, reusing the build path's
// wiring) for this drive's goal and runs the project loop's single slice through
// the supervisor. The shared ledger is injected so the supervised drive charges
// the SAME conversation ceiling as every other drive (§6).
//
// A supervised drive is a multi-agent fan-out: the steer/queue Inbox and the live
// Emitter are first-class on the NATIVE loop (the primary conversational path),
// whereas the supervisor's spawn/code/integrate work is bounded by the rails and
// gated by the verifier (I2) and the single human promote. Wiring the planner's
// own Inbox/Out is a documented follow-on (it needs buildStack to expose the
// supervisor it constructs); here the supervised drive runs to a verifier-green
// tree under the conversation budget, and its outcome folds back into the
// conversation exactly like a native drive.
func chatSuperviseRun(d chatDeps, ledger *budget.Ledger) session.RunSuperviseFunc {
	return func(ctx context.Context, goal string, _ []model.Message, _ session.InboxHandle, _ emit.Emitter) (session.DriveOutcome, error) {
		stack, err := buildStack(chatBuildDeps(d, ledger, goal))
		if err != nil {
			return session.DriveOutcome{}, err
		}
		o, err := stack.loop.Run(ctx)
		if err != nil {
			return session.DriveOutcome{}, err
		}
		return session.DriveOutcome{Summary: o.Summary, Branch: o.Branch, Verified: o.Done}, nil
	}
}

// chatProjectRun returns the RunProjectFunc the project driver invokes: it
// assembles the whole-project stack (via buildStack) for this drive's goal and runs
// the project loop to a verifier-green tree. The shared ledger keeps the project
// drive on the one conversation ceiling (§6). Like the supervised drive, the
// planner's Inbox/Out wiring is a documented follow-on; the drive itself runs
// bounded, verifier-gated, and charged against the conversation wall.
func chatProjectRun(d chatDeps, ledger *budget.Ledger) session.RunProjectFunc {
	return func(ctx context.Context, goal string, _ summarize.ContextSummary, _ emit.Emitter) (session.DriveOutcome, error) {
		stack, err := buildStack(chatBuildDeps(d, ledger, goal))
		if err != nil {
			return session.DriveOutcome{}, err
		}
		o, err := stack.loop.Run(ctx)
		if err != nil {
			return session.DriveOutcome{}, err
		}
		return session.DriveOutcome{Summary: o.Summary, Branch: o.Branch, Verified: o.Done}, nil
	}
}

// chatBuildDeps adapts the chat deps to a buildDeps for buildStack, pinning the
// shared conversation ledger so the supervised/project drive charges the SAME
// ceiling (§6). It mirrors buildMain's defaults for the multi-agent rails sized for
// an interactive drive.
func chatBuildDeps(d chatDeps, ledger *budget.Ledger, goal string) buildDeps {
	adv := resolveAdvisor("native", d.boot, d.flags.common)
	strong := adv.prov
	if strong == nil {
		strong = d.provider
	}
	return buildDeps{
		goal:     goal,
		dir:      d.baseRepo,
		runtime:  *d.flags.common.runtime,
		image:    *d.flags.common.image,
		verify:   *d.flags.common.checkCmd,
		maxIter:  defaultChatMaxIter,
		maxFan:   defaultChatMaxFanout,
		maxAgent: defaultChatMaxAgents,
		maxDepth: 1,
		maxSteps: *d.flags.common.maxSteps,
		budget:   *d.flags.budget,
		executor: d.provider,
		strong:   strong,
		log:      d.log,
		approver: policy.NewConsoleApprover(os.Stdin, os.Stdout),
		ledger:   ledger, // pin the conversation wall (§6)
	}
}

// Multi-agent rail defaults for an interactive chat drive: smaller than the build
// command's so a conversational request never fans out into a runaway, but generous
// enough to ship a multi-step feature.
const (
	defaultChatMaxIter   = 8
	defaultChatMaxFanout = 4
	defaultChatMaxAgents = 16
)

// chatREPL is the line-based stdin reader loop (§5.3). It runs while the agent
// works: each line read from in is handed to Session.Turn, which (under the
// session lock) either routes a new drive (Idle) or pushes the line to the running
// loop's Inbox as a queue/steer (Working). A plain line queues; a '!'/'/steer' line
// steers. /status and /quit are local controls. It returns on EOF (Ctrl-D), on a
// /quit, or when ctx is cancelled (Ctrl-C) — never leaving a reader goroutine
// blocked on a closed session. It is split out (reader + writer injected) so the
// hermetic test drives it with scripted input and a fake session, asserting the
// queue-vs-steer routing without a live model run.
func chatREPL(ctx context.Context, sess chatSession, in io.Reader, out io.Writer) error {
	lines := make(chan string)
	readErr := make(chan error, 1)

	// The reader goroutine blocks on Scan (which has no ctx), so it is detached and
	// drained via the lines channel; the select on ctx.Done lets the loop exit even
	// while the scanner is parked on a blocking read. The goroutine exits on EOF or
	// when the process tears down stdin.
	go func() {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // tolerate a long pasted instruction
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		readErr <- sc.Err()
	}()

	fmt.Fprint(out, chatPromptGlyph)
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "\nshutting down…")
			return nil
		case err := <-readErr:
			fmt.Fprintln(out)
			return err // nil on clean EOF (Ctrl-D)
		case line := <-lines:
			cmd, handled := parseChatLine(line)
			if handled {
				if quit := runChatCommand(ctx, sess, cmd, out); quit {
					return nil
				}
				fmt.Fprint(out, chatPromptGlyph)
				continue
			}
			if strings.TrimSpace(line) == "" {
				fmt.Fprint(out, chatPromptGlyph)
				continue
			}
			// Echo a short ack so the user always knows which mode was understood,
			// BEFORE Turn dispatches (Turn may launch a drive that streams reasoning).
			ackChatMode(out, line)
			if err := sess.Turn(ctx, line); err != nil {
				fmt.Fprintf(out, "  (routing failed: %v)\n", err)
			}
			fmt.Fprint(out, chatPromptGlyph)
		}
	}
}

// chatPromptGlyph is the input prompt. Kept short so agent reasoning lines (which
// carry their own glyphs from the WriterEmitter) read cleanly interleaved.
const chatPromptGlyph = "» "

// chatSession is the minimal Session surface the REPL drives, so the hermetic test
// can substitute a fake that records Turn calls (line + classified mode) without
// constructing the full machinery. *session.Session satisfies it.
type chatSession interface {
	Turn(ctx context.Context, text string) error
	PhaseNow() session.Phase
	Cancel() bool
}

// parseChatLine recognizes the local REPL control verbs (/status, /quit, /help).
// It returns (verb, handled). A '/steer …' line is NOT a control verb — it is a
// steer message for the agent (classified by the session's classifyInterrupt on
// Turn), so it is deliberately left unhandled here and flows to Turn. handled is
// false for every ordinary message and steer.
func parseChatLine(line string) (cmd string, handled bool) {
	switch strings.TrimSpace(line) {
	case "/quit", "/exit":
		return "quit", true
	case "/cancel", "/stop":
		return "cancel", true
	case "/status":
		return "status", true
	case "/help", "/?":
		return "help", true
	default:
		return "", false
	}
}

// runChatCommand executes a local control verb and reports whether the REPL should
// quit. It touches the session only through the read-only PhaseNow accessor (a
// status read never mutates conversation state).
func runChatCommand(_ context.Context, sess chatSession, cmd string, out io.Writer) (quit bool) {
	switch cmd {
	case "quit":
		fmt.Fprintln(out, "bye.")
		return true
	case "cancel":
		// Abort the current run but stay in the conversation (distinct from queue /
		// steer and from Ctrl-C, which exits). Cancel blocks until the drive unwinds.
		if sess.PhaseNow() == session.Idle {
			fmt.Fprintln(out, "  nothing running.")
			return false
		}
		fmt.Fprintln(out, "  cancelling current run…")
		if sess.Cancel() {
			fmt.Fprintln(out, "  cancelled — back to you.")
		}
		return false
	case "status":
		fmt.Fprintf(out, "  status: %s\n", sess.PhaseNow())
		return false
	case "help":
		fmt.Fprintln(out, chatBanner)
		return false
	default:
		return false
	}
}

// ackChatMode prints the queue/steer acknowledgement for a message line BEFORE it
// is dispatched, so the user always knows which mode was understood (§5.3). It
// reuses the session's own queue-vs-steer rule via chatIsSteer so the ack can never
// drift from what Turn actually does.
func ackChatMode(out io.Writer, line string) {
	if chatIsSteer(line) {
		fmt.Fprintln(out, "  steering — interrupting the current step…")
		return
	}
	fmt.Fprintln(out, "  queued (delivered after this step)")
}

// chatIsSteer mirrors session.classifyInterrupt's prefix rule (a leading '!' or a
// '/steer' command marks a steer; everything else queues) so the terminal ack
// matches the mode Turn will assign. It is intentionally a local copy of the rule
// rather than a call into the session, so the REPL stays a thin shell over the
// public Turn entry point.
func chatIsSteer(line string) bool {
	t := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(t, "!") {
		return true
	}
	return t == "/steer" || strings.HasPrefix(t, "/steer ")
}
