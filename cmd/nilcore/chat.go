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
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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
	"nilcore/internal/termui"
	"nilcore/internal/tools"
	"nilcore/internal/verb"
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
	common      commonFlags
	budget      *float64
	allowEgress *string
}

// chatMain is the `nilcore chat` entry point and the bare-`nilcore` default. It
// resolves boot context, builds ONE Session wired to the full machinery, and runs
// the line-based REPL until EOF (Ctrl-D) or interrupt (Ctrl-C). The REPL never
// touches a container or a model directly — every effect flows through the
// Session's drivers, exactly as serve/build do.
func chatMain(args []string) {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	cf := chatFlags{
		common:      registerCommon(fs),
		budget:      fs.Float64("budget", chatDefaultBudget, "global dollar ceiling for the whole conversation (a hard wall via the meter)"),
		allowEgress: fs.String("allow-egress", "", "comma-separated host allowlist for sandboxed web access (e.g. \"example.com,*.docs.io\"); empty = no network (default-deny). Enables the web_fetch tool."),
	}
	_ = fs.Parse(args)

	b := loadBoot(*cf.common.config)
	applyConfigDefaults(cf.common, b.cfg, flagsSet(fs))

	absDir := mustAbs(*cf.common.dir)
	setupMCP(absDir) // generate on-demand MCP wrappers if servers are configured
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

	// The styled terminal renderer: a Console owns the live spinner / streaming
	// line, and a ConsoleEmitter (wired as the session's reasoning sink) turns the
	// loops' live events into the animated surface. On a non-TTY both degrade to
	// clean plain lines (SSH / piped / dumb terminal), invariant I6.
	console := termui.New(os.Stdout)
	emitter := termui.NewEmitter(console, verb.General)

	// Ctrl-C / SIGTERM cancels the whole conversation ctx, so the in-flight drive
	// unwinds to a clean interrupted result and the REPL exits. (To abort only the
	// current run and keep talking, use /cancel — see runChatCommand.)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Sandboxed web access (-allow-egress): default-deny stays the norm — only when
	// the operator passes an allowlist do we stand up the allowlist proxy and enable
	// the web_fetch tool. The proxy is bound to the conversation ctx, so it shuts
	// down when the REPL exits. A namespace-backend sandbox has no proxy egress path
	// (CLONE_NEWNET with no interfaces), so web access requires the container backend.
	egress, proxyAddr, stopProxy := startEgress(ctx, *cf.allowEgress, console)
	defer stopProxy()

	sess, err := buildChatSession(chatDeps{
		flags:           cf,
		provider:        prov,
		boot:            b,
		log:             log,
		baseRepo:        absDir,
		emitter:         emitter,
		egress:          egress,
		egressProxyAddr: proxyAddr,
	})
	if err != nil {
		fatal(err)
	}

	console.Line(console.Style().Dim(chatBanner))
	if err := chatREPL(ctx, sess, os.Stdin, console, emitter); err != nil && err != io.EOF {
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
  /discuss /plan    set a mode (read-only: research & talk / research & plan)
  /execute /auto    set a mode (full capability / let the agent infer scope — default)
                    a mode sticks until you change it; "/plan <text>" sets it and asks
  /mode             show the current mode
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
	emitter  emit.Emitter    // the reasoning sink (REPL: a termui.ConsoleEmitter; TUI: its own; nil ⇒ none)
	approver policy.Approver // irreversible-action gate (nil ⇒ the console approver)

	// egress is the sandbox network allowlist (from -allow-egress). Empty ⇒ default-
	// deny: no network, and the web_fetch tool is not advertised. egressProxyAddr is
	// the host:port of the running allowlist proxy (set when egress is non-empty), fed
	// to a container box via AllowEgressVia so a denied host is simply unreachable (I4).
	egress          policy.Egress
	egressProxyAddr string
}

// webEnabled reports whether sandboxed web access is configured (a non-empty
// allowlist and a running proxy). When false the web_fetch tool is not advertised
// and the sandbox stays --network none (default-deny).
func (d chatDeps) webEnabled() bool {
	return !d.egress.Empty() && d.egressProxyAddr != ""
}

// approverOr returns the wired approver, or the console approver when none is set
// (the line-REPL default). The TUI injects its own modal approver so a gate never
// fights the alt-screen for stdin.
func (d chatDeps) approverOr() policy.Approver {
	if d.approver != nil {
		return d.approver
	}
	return policy.NewConsoleApprover(os.Stdin, os.Stdout)
}

// startEgress resolves the -allow-egress allowlist and, when non-empty, stands up
// the allowlist proxy bound to ctx. It returns the resolved Egress, the proxy's
// bound host:port (empty when egress is off), and an idempotent stop func (a no-op
// when off). Default-deny is preserved: an empty/blank allowlist yields no proxy,
// no network, and no web_fetch tool. The proxy binds to 0.0.0.0 so a bridged
// sandbox container can reach it across the bridge; it only ever forwards to the
// allowlisted hosts and refuses private/loopback destinations (the SSRF guard), so
// it is never an open relay.
func startEgress(ctx context.Context, allow string, con *termui.Console) (policy.Egress, string, func()) {
	hosts := splitHosts(allow)
	if len(hosts) == 0 {
		return policy.Egress{}, "", func() {}
	}
	egress := policy.Egress{Allowed: hosts}
	proxy := &policy.EgressProxy{Egress: egress}
	addr, stop, err := proxy.Start(ctx, "0.0.0.0:0")
	if err != nil {
		// Fail-closed: if the proxy cannot bind, run with no egress rather than
		// silently leaving the sandbox networked.
		con.Line(con.Style().Warn("  web access disabled: could not start egress proxy: " + err.Error()))
		return policy.Egress{}, "", func() {}
	}
	con.Line(con.Style().Dim(fmt.Sprintf("  web access enabled for %d host(s) via the allowlist proxy", len(hosts))))
	return egress, addr, stop
}

// splitHosts parses a comma-separated host allowlist, trimming blanks.
func splitHosts(s string) []string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// applyContainerEgress routes a CONTAINER box through the allowlist proxy. The
// namespace backend has no proxy egress path (it runs in a fresh empty network
// namespace), so it is left untouched and web_fetch fails closed there — egress is
// a container-backend capability. The container reaches the host-side proxy via the
// runtime's host alias (host.containers.internal for podman, host.docker.internal
// for docker, with an --add-host so it resolves on docker-Linux too).
func applyContainerEgress(box sandbox.Sandbox, egress policy.Egress, proxyAddr, runtime string) {
	if egress.Empty() || proxyAddr == "" {
		return
	}
	c, ok := box.(*sandbox.Container)
	if !ok {
		return
	}
	_, port, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		return
	}
	hostAlias := "host.containers.internal" // podman (rootless) provides this by default
	if runtime == "docker" {
		hostAlias = "host.docker.internal"
		c.ExtraHosts = append(c.ExtraHosts, "host.docker.internal:host-gateway")
	}
	c.AllowEgressVia(policy.ProxyURL(net.JoinHostPort(hostAlias, port)))
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
	// Live reasoning/tokens render through the styled Console (the steer surface,
	// §5.3). A nil emitter leaves Out unset — the loops gate on it and stay
	// byte-identical, so the hermetic test wires no sink.
	if d.emitter != nil {
		sess.Out = d.emitter
	}
	sess.Budget = ledger

	sess.Router = &session.SupervisorFirstRouter{
		Classifier:      metered,
		ShouldSupervise: chatShouldSupervise,
		Log:             d.log,
		ID:              chatConvoID,
	}
	// Sizer is consulted ONLY when the user pins /execute (which bypasses the
	// router): it sizes native-vs-supervise with the SAME heuristic the router
	// reconciles against, so a large execute request still fans out to the supervisor.
	sess.Sizer = chatShouldSupervise

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

// capabilityForMode is the per-mode enforcement seam (the load-bearing one): it
// returns the tool registry, the `run` command guard, and the shell switch a chat
// native drive is built with. Execute and Auto get the full write set + default
// guard + shell — today's behavior, byte-identical. The read-only modes
// (Discuss/Plan) get the WRITE-FREE read/search/codeintel set, the read-only guard,
// and the shell OFF, so a read-only drive has no registered path to mutate the tree
// regardless of what the model attempts — capability via wiring, not via prompt (I7).
func capabilityForMode(m session.Mode) (reg *tools.Registry, guard func(string) (bool, string), disableShell bool) {
	if m.ReadOnly() {
		return readOnlyLoopTools(), policy.ReadOnlyCommandPolicy().Check, true
	}
	return loopTools(), policy.DefaultCommandPolicy().Check, false
}

// readOnlyLoopTools is the read-only counterpart of loopTools: the shared
// write-free read/search/codeintel set (tools.ReadOnlyWithCodeintel) plus any
// installed Agent Skills (skill tools only RETURN instructions — they carry no
// write surface, so they keep the structural read-only guarantee intact). It is
// what Discuss/Plan drives advertise.
func readOnlyLoopTools() *tools.Registry {
	r := tools.ReadOnlyWithCodeintel()
	for _, t := range skillTools() {
		r.Register(t)
	}
	return r
}

// modePreamble is the harness-authored framing prepended to a read-only drive's
// goal so the model knows it is researching (not implementing) and must end by
// calling finish with its plan/answer. Empty for Execute/Auto (byte-identical goal).
// It is principal-trusted framing, never untrusted data.
func modePreamble(m session.Mode) string {
	switch m {
	case session.ModePlan:
		return "[PLAN MODE — read-only] Research the codebase (read/search/codeintel) and the request, then " +
			"produce a detailed, inspectable implementation plan: the approach, the files/functions to touch, " +
			"trade-offs, and how 'done' will be verified. You CANNOT write or run code — there are no " +
			"write/edit/git tools and no shell. When the plan is ready, call finish with the plan as the summary.\n\n"
	case session.ModeDiscuss:
		return "[DISCUSS MODE — read-only] Converse with the user about the request: offer pros and cons, best " +
			"practices, and insight grounded in this codebase (use read/search/codeintel to ground your points). " +
			"You CANNOT write or run code. When you have answered, call finish with a brief recap.\n\n"
	default:
		return ""
	}
}

// modeBlurb is the one-line description shown when the user switches mode, so the
// control surface explains what each mode does without a docs trip.
func modeBlurb(m session.Mode) string {
	switch m {
	case session.ModeDiscuss:
		return " — research & converse; no code is written"
	case session.ModePlan:
		return " — research & plan in depth; no code is written"
	case session.ModeExecute:
		return " — full capability: write, run, and verify code"
	default:
		return " — the agent infers scope (quick fix / feature / project)"
	}
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
		//
		// The mode (captured in NativeRun at launch) governs CAPABILITY: a read-only
		// Discuss/Plan drive gets a write-free, shell-off backend and a pass-through
		// verifier (a research turn ships nothing, so there is nothing to gate, I2);
		// Execute/Auto get the full write-capable backend gated by the real verifier.
		newEnv := func(dir string) agent.Env {
			box := selectSandbox(*d.flags.common.sandboxPref, *d.flags.common.runtime, *d.flags.common.image, dir)
			// Route a container box through the allowlist proxy when web access is on
			// (no-op otherwise; default-deny stays the norm).
			applyContainerEgress(box, d.egress, d.egressProxyAddr, *d.flags.common.runtime)
			var v verify.Verifier = verify.New(box, *d.flags.common.checkCmd)
			if in.Mode.ReadOnly() {
				v = verify.Pass{}
			}
			n := chatNativeBackend(d, metered, adv, box, v, in)
			return agent.Env{Backend: n, Verifier: v}
		}

		orch := &agent.Orchestrator{
			BaseRepo: d.baseRepo,
			NewEnv:   newEnv,
			Log:      d.log,
			Router:   agent.SingleRouter{},
			Spawner:  agent.NoSpawner{},
			Approver: d.approverOr(),
		}

		// The mode preamble is harness-authored, principal-trusted framing prepended
		// to the goal so a read-only Discuss/Plan drive knows it is researching (and
		// must finish with a plan/answer rather than expecting to write). It is empty
		// for Execute/Auto, so those goals are byte-identical.
		out, err := orch.Execute(ctx, backend.Task{ID: in.TaskID, Goal: modePreamble(in.Mode) + in.Goal})
		if err != nil {
			return session.DriveOutcome{}, err
		}
		emitDriveResult(in.Emitter, out.Verified, out.Summary)
		return session.DriveOutcome{Summary: out.Summary, Verified: out.Verified}, nil
	}
}

// emitDriveResult surfaces a drive's terminal outcome through the live emitter as
// a verify line (✓/✗ by content) — the session folds the result into State but
// does not itself emit it, so the conversation would otherwise not show the
// conclusion. nil emitter (the non-conversational paths) ⇒ no-op.
func emitDriveResult(out emit.Emitter, verified bool, summary string) {
	if out == nil {
		return
	}
	s := strings.TrimSpace(summary)
	if s == "" {
		s = "done"
	}
	if verified {
		out.Emit(emit.Event{Kind: emit.KindVerify, Text: "verified — " + s})
	} else {
		out.Emit(emit.Event{Kind: emit.KindVerify, Text: "not verified — " + s})
	}
}

// chatNativeBackend builds the backend.Native for one chat drive with the
// conversational seams attached: the session Inbox (steer/queue), Seed (the prior
// History — continue, not restart), and Emitter (live reasoning). It mirrors
// buildBackend's native arm but threads the three additive gates; with a nil
// Inbox/Emitter the loop would be byte-identical to a plain run (here they are
// always wired, since chat is the seam's reason to exist).
func chatNativeBackend(d chatDeps, prov model.Provider, adv advisorCfg, box sandbox.Sandbox, v verify.Verifier, in session.NativeRun) *backend.Native {
	// capabilityForMode is the enforcement seam: a read-only mode gets a write-free
	// registry, the read-only command guard, and the shell switched OFF — so "writes
	// no code" is structural (no registered write/edit/git tool and no shell escape),
	// not a prompt the model might ignore (I7). Execute/Auto get today's full set.
	reg, guard, disableShell := capabilityForMode(in.Mode)
	// The sandboxed web_fetch tool is wired in here (it must bind to THIS drive's
	// box, which only exists now) when web access is enabled. It is read-only/
	// non-mutating — it fetches a URL inside the box under the egress allowlist and
	// fences the body as untrusted data (I7) — so it preserves the write-free
	// guarantee even in the read-only Discuss/Plan modes. Without -allow-egress it is
	// never advertised, so the tool surface stays honest (a fetch would fail closed).
	if d.webEnabled() {
		if _, ok := box.(*sandbox.Container); ok {
			reg.Register(tools.WebFetchTool{Box: box})
		}
	}
	n := &backend.Native{
		Model:        prov,
		Box:          box,
		Verifier:     v,
		Log:          d.log,
		Tools:        reg,
		CommandGuard: guard,
		DisableShell: disableShell,
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
	return func(ctx context.Context, goal string, _ []model.Message, _ session.InboxHandle, outEmitter emit.Emitter) (session.DriveOutcome, error) {
		stack, err := buildStack(chatBuildDeps(d, ledger, goal))
		if err != nil {
			return session.DriveOutcome{}, err
		}
		o, err := stack.loop.Run(ctx)
		if err != nil {
			return session.DriveOutcome{}, err
		}
		emitDriveResult(outEmitter, o.Done, o.Summary)
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
	return func(ctx context.Context, goal string, _ summarize.ContextSummary, outEmitter emit.Emitter) (session.DriveOutcome, error) {
		stack, err := buildStack(chatBuildDeps(d, ledger, goal))
		if err != nil {
			return session.DriveOutcome{}, err
		}
		o, err := stack.loop.Run(ctx)
		if err != nil {
			return session.DriveOutcome{}, err
		}
		emitDriveResult(outEmitter, o.Done, o.Summary)
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
		approver: d.approverOr(),
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
func chatREPL(ctx context.Context, sess chatSession, in io.Reader, con *termui.Console, em *termui.ConsoleEmitter) error {
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

	// The bottom line is EITHER the prompt (Idle, awaiting input) OR the live
	// spinner/stream (Working). `working` tracks which: a drive going Working starts
	// the thinking spinner and hides the prompt; settling stops the spinner and
	// re-offers it. Only this goroutine touches `working`.
	working := false
	prompt := func() { con.Prompt(con.Style().Info(chatPromptGlyph)) }
	settle := func() {
		if working {
			working = false
			em.End()
		}
	}
	// reconcile shows the spinner for a fresh drive or the prompt when idle, after
	// every line/command that may have changed the phase.
	reconcile := func() {
		if sess.PhaseNow() == session.Working {
			if !working {
				working = true
				em.Begin(verb.General)
			}
			return
		}
		settle()
		prompt()
	}

	prompt()
	// A slow tick catches a drive that finishes asynchronously (the REPL is parked
	// on the select, not on the drive) so the prompt returns promptly after work.
	tick := time.NewTicker(120 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			settle()
			con.Line("shutting down…")
			return nil
		case err := <-readErr:
			settle()
			con.Line("")
			return err // nil on clean EOF (Ctrl-D)
		case <-tick.C:
			if working && sess.PhaseNow() == session.Idle {
				settle()
				prompt()
			}
		case line := <-lines:
			if mode, rest, ok := parseModeVerb(line); ok {
				// A mode control verb (/discuss /plan /execute /auto), optionally
				// followed by a request on the same line ("/plan add a limiter").
				applyModeVerb(ctx, sess, con, mode, rest)
			} else if cmd, handled := parseChatLine(line); handled {
				if quit := runChatCommand(ctx, sess, cmd, con); quit {
					settle()
					return nil
				}
			} else if strings.TrimSpace(line) != "" {
				// Ack the mode BEFORE Turn dispatches (it may launch a streaming drive).
				ackChatMode(con, line)
				if err := sess.Turn(ctx, line); err != nil {
					con.Line(con.Style().Dim("  (routing failed: " + err.Error() + ")"))
				}
			}
			reconcile()
		}
	}
}

// chatPromptGlyph is the input prompt. Kept short so agent reasoning lines (which
// carry their own glyphs) read cleanly interleaved; styled cyan by the Console.
const chatPromptGlyph = "❯ "

// chatSession is the minimal Session surface the REPL drives, so the hermetic test
// can substitute a fake that records Turn calls (line + classified mode) without
// constructing the full machinery. *session.Session satisfies it.
type chatSession interface {
	Turn(ctx context.Context, text string) error
	PhaseNow() session.Phase
	Cancel() bool
	SetMode(session.Mode)
	CurrentMode() session.Mode
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
	case "/mode":
		return "mode", true
	case "/help", "/?":
		return "help", true
	default:
		return "", false
	}
}

// chatModeVerbs maps each mode control verb to the Mode it pins.
var chatModeVerbs = map[string]session.Mode{
	"/discuss": session.ModeDiscuss,
	"/plan":    session.ModePlan,
	"/execute": session.ModeExecute,
	"/auto":    session.ModeAuto,
}

// parseModeVerb recognizes a leading mode control verb and returns the Mode it
// pins plus any trailing text, so "/plan add a limiter" both switches to plan mode
// AND submits the request as a turn. ok is false for any line not starting with a
// mode verb. These are PRINCIPAL controls, parsed ONLY here at the front door —
// never from Turn text, an inbox follow-up, or tool output — so untrusted content
// can never flip the mode (I7). "/mode" (no target) is a status read, handled as a
// control verb in parseChatLine, not here.
func parseModeVerb(line string) (mode session.Mode, rest string, ok bool) {
	t := strings.TrimSpace(line)
	first := t
	if i := strings.IndexAny(t, " \t"); i >= 0 {
		first = t[:i]
		rest = strings.TrimSpace(t[i+1:])
	}
	m, found := chatModeVerbs[first]
	if !found {
		return session.ModeAuto, "", false
	}
	return m, rest, true
}

// applyModeVerb pins the mode, acks it, and — if the verb carried trailing text —
// submits that text as a turn under the new mode. A switch while a drive is Working
// applies only to the NEXT turn (the running drive's capability is fixed at launch),
// so it says so when there is no trailing request to act on immediately.
func applyModeVerb(ctx context.Context, sess chatSession, con *termui.Console, mode session.Mode, rest string) {
	st := con.Style()
	working := sess.PhaseNow() != session.Idle
	sess.SetMode(mode)
	note := ""
	if working && rest == "" {
		note = " (applies to your next turn; the current run keeps its capability)"
	}
	con.Line(st.Info("  mode → " + mode.String() + modeBlurb(mode) + note))
	if rest != "" {
		ackChatMode(con, rest)
		if err := sess.Turn(ctx, rest); err != nil {
			con.Line(st.Dim("  (routing failed: " + err.Error() + ")"))
		}
	}
}

// runChatCommand executes a local control verb and reports whether the REPL should
// quit. It touches the session only through the read-only PhaseNow accessor (a
// status read never mutates conversation state).
func runChatCommand(_ context.Context, sess chatSession, cmd string, con *termui.Console) (quit bool) {
	st := con.Style()
	switch cmd {
	case "quit":
		con.Line("bye.")
		return true
	case "cancel":
		// Abort the current run but stay in the conversation (distinct from queue /
		// steer and from Ctrl-C, which exits). Cancel blocks until the drive unwinds.
		if sess.PhaseNow() == session.Idle {
			con.Line(st.Dim("  nothing running."))
			return false
		}
		con.Line(st.Warn("  cancelling current run…"))
		if sess.Cancel() {
			con.Line(st.Warn("  cancelled — back to you."))
		}
		return false
	case "status":
		con.Line(st.Dim(fmt.Sprintf("  status: %s · mode: %s", sess.PhaseNow(), sess.CurrentMode())))
		return false
	case "mode":
		m := sess.CurrentMode()
		con.Line(st.Info("  mode: " + m.String() + modeBlurb(m)))
		return false
	case "help":
		con.Line(st.Dim(chatBanner))
		return false
	default:
		return false
	}
}

// ackChatMode prints the queue/steer acknowledgement for a message line BEFORE it
// is dispatched, so the user always knows which mode was understood (§5.3). It
// reuses the session's own queue-vs-steer rule via chatIsSteer so the ack can never
// drift from what Turn actually does.
func ackChatMode(con *termui.Console, line string) {
	st := con.Style()
	if chatIsSteer(line) {
		con.Line(st.Warn("  steering — interrupting the current step…"))
		return
	}
	con.Line(st.Dim("  queued (delivered after this step)"))
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
