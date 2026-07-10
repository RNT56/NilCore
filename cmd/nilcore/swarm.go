// swarm.go wires the `nilcore swarm` subcommand (docs/SWARM.md §8, SW-T17): the
// final integration that turns the committed Phase-12 leaf packages — the provider
// pool, the verify-pack assembler, the preset catalog, the swarm Runner/Controller,
// and the live scoreboard board — into a working verified-swarm command. From one
// goal (or an operator shard list) it fans N shards into a bounded, metered pool,
// each shard producing a TYPED artifact whose claims a per-shard verifier re-checks
// in-box (I2), requeues only the still-red claims until clean, and surfaces the final
// clean tip as a single gated PromoteToBase candidate (never auto-landed).
//
// It is purely WIRING: every capability already ships in internal/. swarm.go is
// `package main` exactly like build.go/main.go/report.go, so it REUSES the existing
// cmd helpers directly rather than reinventing them — loadBoot/openLog, the
// SecretStore-backed cred resolver, selectSandbox, buildEnvFactory, meterProvider,
// buildBackend (delegated codex/claude-code in-box), buildIntegrateFunc,
// buildGateFunc, readVerifiedArtifact, preserveFailedAttempt, and the report.go
// swarm rendering path. Four properties are load-bearing and shape the wiring:
//
//   - ONE shared *budget.Ledger across every metered provider (planner, verifier,
//     and all worker shards). pool.Build wraps the tiers; the ledger's global ceiling
//     is the hard dollar wall — a runaway aborts via budget.ErrCeiling, which the
//     Controller treats as a termination rail, never a done-signal.
//   - The verifier is the ONLY authority on done (I2). Each shard is judged by the
//     packs.Build verifier (schema + per-claim in-box ArtifactVerifier, plus a raw
//     build/browser child for code/ui) wrapped in swarm.NewShipGate, which refuses a
//     vacuous verify.Pass{}/nil. Passed is the verdict, NEVER the worker self-report.
//   - The single human gate is the final promote: one policy.GateAction{PromoteToBase}
//     through buildGateFunc (a nil approver default-denies). The swarm NEVER auto-lands.
//   - Default-off, byte-identical: main.go gains only `case "swarm"` + usage lines;
//     all logic lives here. The default dispatch path reaches neither new arm.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/packs"
	"nilcore/internal/backend"
	"nilcore/internal/budget"
	"nilcore/internal/eventlog"
	"nilcore/internal/integrate"
	"nilcore/internal/meter"
	"nilcore/internal/paths"
	"nilcore/internal/planner"
	"nilcore/internal/policy"
	"nilcore/internal/pool"
	"nilcore/internal/requeue"
	"nilcore/internal/roster"
	"nilcore/internal/spawn"
	"nilcore/internal/store"
	"nilcore/internal/swarm"
	"nilcore/internal/swarm/board"
	"nilcore/internal/swarm/preset"
	"nilcore/internal/termui"
	"nilcore/internal/worktree"
)

// swarmFlags are the `nilcore swarm` flags (docs/SWARM.md §8.2). They reuse
// registerCommon for -dir/-sandbox/-runtime/-image/-log/-config and add the swarm-
// specific surface. Defaults match §8.2: preset=research, concurrency=1,
// passes=until-clean, budget=25.00, jitter=750ms, report=text.
type swarmFlags struct {
	common commonFlags

	goal        *string
	preset      *string
	shardFile   *string
	agents      *int
	concurrency *int
	artifact    *string
	verifyPack  *string
	passes      *string
	retries     *int
	budget      *float64
	perShard    *float64

	workerModel   *string
	plannerModel  *string
	verifyModel   *string
	fallbackModel *string
	codeBackend   *string
	providerCap   providerCaps
	jitter        *time.Duration
	egressAllow   *string

	reportFmt *string
	resume    *bool
	out       *string
	deadline  *time.Duration
}

// providerCaps collects the repeatable --provider-cap K=V flag into a map. It
// satisfies flag.Value so each occurrence on the command line adds one entry.
type providerCaps map[string]int

func (p providerCaps) String() string {
	if len(p) == 0 {
		return ""
	}
	parts := make([]string, 0, len(p))
	for k, v := range p {
		parts = append(parts, fmt.Sprintf("%s=%d", k, v))
	}
	return strings.Join(parts, ",")
}

func (p providerCaps) Set(v string) error {
	i := strings.IndexByte(v, '=')
	if i <= 0 || i == len(v)-1 {
		return fmt.Errorf("want provider:model=N, got %q", v)
	}
	n, err := strconv.Atoi(strings.TrimSpace(v[i+1:]))
	if err != nil {
		return fmt.Errorf("cap for %q: %w", v[:i], err)
	}
	p[strings.TrimSpace(v[:i])] = n
	return nil
}

func registerSwarmFlags(fs *flag.FlagSet) swarmFlags {
	caps := providerCaps{}
	sf := swarmFlags{
		common:        registerCommon(fs),
		goal:          fs.String("goal", "", "the swarm objective, in plain language"),
		preset:        fs.String("preset", "research", "preset bundle: "+strings.Join(preset.Names(), " | ")),
		shardFile:     fs.String("shard-file", "", "operator shard list, one unit per line (selects the ListSharder)"),
		agents:        fs.Int("agents", 0, "target shard count for the PlanSharder (0 = the planner decides)"),
		concurrency:   fs.Int("concurrency", 1, "pool cap on simultaneously-running shards (1 = serial, the byte-identical default)"),
		artifact:      fs.String("artifact", "", "'+'-joined deliverables: report+matrix | spec | benchmark | dossier | json (default = the preset's Kind)"),
		verifyPack:    fs.String("verify-pack", "", "override the preset's verify pack(s) (comma-separated)"),
		passes:        fs.String("passes", "until-clean", "requeue passes: until-clean | <N>"),
		retries:       fs.Int("retries", 2, "focused-retry budget per red claim / merge conflict (0 = a red claim is final)"),
		budget:        fs.Float64("budget", 25.00, "global dollar ceiling for the whole run (a hard wall via the meter)"),
		perShard:      fs.Float64("per-shard-budget", 0, "per-shard dollar ceiling (0 = no per-shard cap)"),
		workerModel:   fs.String("worker-model", "", "provider:model for the cheap worker tier (empty = pool default)"),
		plannerModel:  fs.String("planner-model", "", "provider:model for the strong planner tier"),
		verifyModel:   fs.String("verify-model", "", "provider:model for the strong verifier tier"),
		fallbackModel: fs.String("fallback-model", "", "provider:model failover target for every tier"),
		codeBackend:   fs.String("code-backend", "native", "coding-shard backend: native | codex | claude-code"),
		jitter:        fs.Duration("jitter", 750*time.Millisecond, "model-call jitter (a large value de-correlates a 300-agent retry storm)"),
		egressAllow:   fs.String("egress-allow", "", "extra comma-separated hosts to widen the shard egress allowlist"),
		reportFmt:     fs.String("report", "text", "final report format: text | md | html | json | matrix"),
		resume:        fs.Bool("resume", false, "resume an interrupted run from the durable queue (re-drive only still-red shards)"),
		out:           fs.String("out", "", "also write the rendered report under .nilcore/reports/<run>.<ext>"),
		deadline:      fs.Duration("deadline", 2*time.Hour, "wall-clock ceiling for the whole run"),
	}
	sf.providerCap = caps
	fs.Var(caps, "provider-cap", "per-provider concurrency cap, K=V (repeatable), e.g. anthropic:claude-opus-4-8=20")
	return sf
}

// swarmMain is the `nilcore swarm` entry point. It parses flags, assembles the stack
// via buildSwarm (kept separate so a hermetic test exercises the wiring without a
// container or a real model), runs the multi-pass Controller, prints the live
// scoreboard, renders the report, and exits 0 iff the run converged green AND the
// event-log chain verifies.
func swarmMain(args []string) {
	fs := flag.NewFlagSet("swarm", flag.ExitOnError)
	sf := registerSwarmFlags(fs)
	_ = fs.Parse(args)

	if *sf.goal == "" && *sf.shardFile == "" {
		fmt.Fprintln(os.Stderr, "error: --goal (or --shard-file) is required\nrun 'nilcore swarm -h' for usage")
		os.Exit(2)
	}

	b := loadBoot(*sf.common.config)
	set := flagsSet(fs)
	applyConfigDefaults(sf.common, b.cfg, set)
	log := openLog(*sf.common.logPath)
	defer log.Close()

	swarmRun(swarmDeps{
		flags:         sf,
		boot:          b,
		log:           log,
		dir:           mustAbs(*sf.common.dir),
		explicitFlags: set, // so an explicit --retries overrides the persisted ledger on --resume
	})
}

// swarmRun assembles + drives a swarm run (buildSwarm → Controller → scoreboard + report)
// and OWNS the process exit. Shared by swarmMain (a goal / --shard-file) and by
// `nilcore flows run` (which injects a pre-built agentic-flows DAG via d.tree). It exits 0
// iff the run converged with an empty worklist AND the report's chain verifies (so a
// tampered log can never read green); otherwise os.Exit(1) after printing the scoreboard.
func swarmRun(d swarmDeps) {
	asm, err := buildSwarm(d)
	if err != nil {
		fatal(err)
	}
	defer asm.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), *d.flags.deadline)
	defer cancel()

	out, runErr := asm.run(ctx)
	if runErr != nil {
		fatal(runErr)
	}

	// Print the final live scoreboard and the requested report, both off the same
	// append-only log + persisted artifacts (the report path report.go already uses).
	fmt.Print(board.RenderScoreboard(asm.board.Snapshot(), asm.style))
	rendered, exit := asm.renderReport()
	fmt.Fprint(os.Stdout, rendered)

	if out.Done && out.Remaining == 0 && exit == 0 {
		return
	}
	os.Exit(1)
}

// swarmDeps is the resolved input to buildSwarm: flags + boot + the open log + the
// repo dir. Keeping it a plain struct lets a hermetic test inject fakes (a scripted
// pool, a temp git repo, a pre-exhausted ledger) and assert the wiring.
type swarmDeps struct {
	flags swarmFlags
	boot  boot
	log   *eventlog.Log
	dir   string

	// ledger, when non-nil, is the shared budget Ledger the whole run charges
	// through. swarmMain leaves it nil (buildSwarm mints one and applies the global
	// ceiling); a hermetic test injects its own pre-exhausted ledger to assert every
	// metered provider charges the ONE wall.
	ledger *budget.Ledger

	// pool, when non-nil, overrides the production pool.Build (a test injects a pool
	// built over scripted providers so no real vendor adapter or network is touched).
	pool *pool.Pool

	// store, when non-nil, overrides the durable queue's store (a test injects an
	// in-memory store so the queue persists without a real data dir).
	store *store.Store

	// tree, when non-nil, is a PRE-BUILT plan the caller already knows (e.g. `nilcore
	// flows run`, whose agentic-flows agent_task DAG IS the plan). When set, buildSwarm
	// shards it via swarm.TreeSharder (no planner model call) instead of the preset's
	// goal-based Sharder — so the caller's DependsOn edges become real Shard.Deps and the
	// runner honors the DAG. nil (swarmMain) ⇒ the flag/preset-based sharder, unchanged.
	tree *planner.Tree

	// explicitFlags is the set of flag names the operator set on THIS invocation's command
	// line (fs.Visit). buildSwarm reads it so an EXPLICIT --retries overrides the persisted
	// ledger budget on --resume (Fix #13): without it, `state = *resumed` wholesale-adopts
	// the old MaxAttempts and silently ignores a fresh --retries. nil ⇒ treat nothing as
	// explicitly set (a test or a caller that did not thread the flag set, e.g. flows run).
	explicitFlags map[string]bool
}

// swarmAssembly is the assembled swarm run: the Controller and the initial shard set
// it drives, the shared ledger and pool (exposed so a test asserts the single-wall
// invariant), the live board, the resolved preset (so a test inspects the routing),
// the run-level deliverable set, and the cleanup hook. repo is the integration repo
// the per-shard worktrees are cut from; collateRoot is the PER-RUN directory
// (.nilcore/swarm/<runID>) requeue.Scan reads every shard's verified artifact from —
// namespaced by runID so a stale artifact from a prior run can never bleed in (MAJOR #2).
type swarmAssembly struct {
	controller *swarm.Controller
	initial    []swarm.Shard
	state      swarm.SwarmState

	flags  swarmFlags
	ledger *budget.Ledger
	pool   *pool.Pool
	board  *board.Board
	preset preset.Preset

	deliverables deliverableSet
	repo         string
	collateRoot  string
	runID        string
	logPath      string

	gate  func(policy.GateAction) bool
	style termui.Style

	cleanup func()
}

// deliverableSet is the parsed --artifact list: the per-shard Kind the shardFn
// enforces (overriding the preset default when given), the run-level report format,
// and whether a cross-shard matrix render is also requested. "matrix" always triggers
// the matrix pivot regardless of per-shard Kind (the headline command's contract).
type deliverableSet struct {
	kind   artifact.Kind // per-shard artifact Kind the shards emit
	report bool          // a text/md/html/json report is rendered at the end
	matrix bool          // RenderMatrix is also rendered at the end
}

// parseDeliverables parses the '+'-joined --artifact list into the per-shard Kind and
// the run-level deliverable set. An empty list defaults to the preset's Kind and a
// single report. "matrix" sets the matrix flag (and, on its own, leaves the per-shard
// Kind at the preset default — a matrix is a cross-shard PIVOT, not a per-shard Kind).
// Recognized tokens map to artifact Kinds; an unknown token is reported so a typo
// fails loudly rather than silently shipping the wrong deliverable.
func parseDeliverables(list string, presetKind artifact.Kind) (deliverableSet, error) {
	d := deliverableSet{kind: presetKind, report: true}
	list = strings.TrimSpace(list)
	if list == "" {
		return d, nil
	}
	d.report = false
	kindSet := false
	for _, raw := range strings.Split(list, "+") {
		tok := strings.ToLower(strings.TrimSpace(raw))
		if tok == "" {
			continue
		}
		switch tok {
		case "matrix":
			d.matrix = true
			d.report = true // matrix is rendered through the report path
		case "json":
			d.report = true
		case "report":
			d.kind, kindSet = artifact.KindReport, true
			d.report = true
		case "spec":
			d.kind, kindSet = artifact.KindSpec, true
			d.report = true
		case "benchmark":
			d.kind, kindSet = artifact.KindBenchmark, true
			d.report = true
		case "dossier":
			d.kind, kindSet = artifact.KindDossier, true
			d.report = true
		default:
			return deliverableSet{}, fmt.Errorf("swarm: unknown --artifact deliverable %q (want report | matrix | spec | benchmark | dossier | json)", tok)
		}
	}
	if !kindSet {
		d.kind = presetKind
	}
	return d, nil
}

// passPolicyFromFlag parses --passes into a swarm.PassPolicy. "until-clean" drives
// requeue rounds until the worklist is empty (or another rail trips); an integer N
// caps the run at N passes. It is fail-closed: an unparseable value is an error at
// startup rather than a silent default.
func passPolicyFromFlag(v string) (swarm.PassPolicy, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" || v == "until-clean" {
		return swarm.PassPolicy{UntilClean: true}, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return swarm.PassPolicy{}, fmt.Errorf("swarm: --passes %q: want until-clean or a positive integer", v)
	}
	return swarm.PassPolicy{MaxPasses: n}, nil
}

// buildSwarm assembles the whole swarm stack. It resolves in the §8.2 order:
// preset (FATAL on an unknown name / unknown verify pack at STARTUP — fail-closed) →
// the SecretStore-backed cred resolver → ONE shared budget.Ledger with the global
// ceiling → pool.Build (every tier metered against that ledger) → the per-shard
// verifier factory (packs.Build wrapped in swarm.NewShipGate) → the preset roster →
// the shardFn I2 closure → the Integrator (FanInMerge) or nil (FanInCollate) → the
// Runner + Controller. It returns the assembly; run() is the caller's call.
func buildSwarm(d swarmDeps) (swarmAssembly, error) {
	sf := d.flags

	// --- preset (fail-closed at startup) ---
	pre, _, err := preset.Resolve(*sf.preset)
	if err != nil {
		return swarmAssembly{}, err // ErrUnknownPreset surfaces here, before any work
	}

	// --artifact: parse the deliverable set + the per-shard Kind override.
	deliverables, err := parseDeliverables(*sf.artifact, pre.Kind)
	if err != nil {
		return swarmAssembly{}, err
	}

	// --verify-pack override: when given, REPLACE the preset's packs. Each named pack
	// must be known (HostsFor + Select are the registry); an unknown pack is FATAL at
	// startup (fail-closed). We validate by attempting a Select into a throwaway
	// registry, the same fail-closed check packs.Build runs per shard.
	packNames := pre.VerifyPacks
	if ov := strings.TrimSpace(*sf.verifyPack); ov != "" {
		packNames = splitCSV(ov)
		if err := validatePacks(packNames); err != nil {
			return swarmAssembly{}, err
		}
	}
	// The shard's governing pack: the FIRST named pack assembles the per-shard
	// verifier (packs.Build composes schema + that pack's per-claim checks + an
	// optional build/browser child). A preset names one or two packs; the first is
	// the primary that gates the Kind.
	if len(packNames) == 0 {
		return swarmAssembly{}, fmt.Errorf("swarm: preset %q has no verify pack", pre.Name)
	}
	shardPack := packNames[0]

	// --- credentials: the SecretStore-backed env→value resolver serveMain builds ---
	cred := d.boot.cred

	// --- ONE shared ledger + global ceiling (the hard dollar wall, §6/§7) ---
	ledger := d.ledger
	if ledger == nil {
		ledger = budget.New()
		ledger.SetGlobalCeiling(*sf.budget)
	}

	// --- the durable queue's store (best-effort: a test injects an in-memory store;
	// production opens the data-dir store like setupPersistence). It is opened BEFORE
	// runID is resolved so a --resume can discover an interrupted run's id from it. ---
	st := d.store
	cleanup := func() {}
	if st == nil {
		s, qcleanup := openSwarmStore(d.log)
		st = s
		cleanup = qcleanup
	}

	// --- runID + (on resume) the prior SwarmState. A fresh run mints a new id; a
	// --resume run adopts the most recent interrupted run's id + state so the queue,
	// the ledger, and the integration tip all continue from where the prior process
	// left off (LOCAL-process-restart resume over the LOCAL store — never cross-host). ---
	runID := swarmRunID()
	var resumed *swarm.SwarmState
	if *sf.resume {
		rs, rerr := loadResumeState(context.Background(), st, d.log)
		if rerr != nil {
			return swarmAssembly{}, rerr
		}
		if rs != nil {
			runID = rs.RunID
			resumed = rs
		} else {
			fmt.Fprintln(os.Stderr, "nilcore swarm: --resume found no interrupted run; starting fresh")
		}
	}
	queue := swarm.NewQueue(st, d.log, runID)

	// --- the live scoreboard board (fed from the Controller's per-pass tally + the
	// pool's per-model OnUsage). Cost is read LIVE from the shared ledger. ---
	bd := board.New(ledger, meter.NewTable(), boardMinInterval)

	// --- the provider pool: one shared ledger across every tier. A test may inject a
	// pre-built pool over scripted providers; production builds from the flags + the
	// optional onboard.Config.Pool. opts.OnUsage fans every model call into the board. ---
	pl := d.pool
	if pl == nil {
		cfg := poolConfigFromFlags(sf, d.boot)
		pl, err = pool.Build(cfg, ledger, cred, runID, pool.Options{
			OnUsage: bd.OnUsage,
			Pricer:  meter.NewTable(),
		})
		if err != nil {
			return swarmAssembly{}, fmt.Errorf("swarm: build pool: %w", err)
		}
	}
	pl.SetGlobalCeiling(*sf.budget)

	// --- the collate root: a PER-RUN directory requeue.Scan reads every shard's verified
	// artifact from, NAMESPACED by runID (.nilcore/swarm/<runID>) so a stale RED artifact
	// from a prior abandoned run can NEVER be ingested by a fresh run — which would make a
	// fully-green fresh run terminate Done=false/exhausted/exit 1 (MAJOR #2). Each shard
	// runs+verifies in its OWN disposable worktree (I4); on green its verified artifact is
	// copied here so the Controller's requeue.Scan (which reads
	// <collateRoot>/.nilcore/artifacts/*.json) sees ONLY this run's artifacts. The
	// integration repo the per-shard worktrees are cut from is --dir (a real git repo for
	// code shards; for collate presets it is only the worktree source). The whole
	// .nilcore/swarm tree is git-ignored, so the persistent repo is never polluted. ---
	repo := d.dir
	collateRoot := filepath.Join(repo, ".nilcore", "swarm", runID)
	if err := os.MkdirAll(filepath.Join(collateRoot, ".nilcore", "artifacts"), 0o755); err != nil {
		return swarmAssembly{}, fmt.Errorf("swarm: collate root: %w", err)
	}

	// --- the per-worktree env factory (sandbox + project verifier), reused from
	// build.go. The project verifier is only the raw build child for code/ui packs;
	// the per-shard governing verifier is the packs.Build composite below. ---
	//
	// FIX 3: wire -blast-radius so the unattended safety envelope actually bounds the run
	// (it was pulled in via registerCommon but never consumed, a silent no-op). The shared
	// blast budget fences every shard's sandbox cumulative WALL-TIME (BR-T03) via attachBlast
	// inside the factory — the SAME meter build/run thread in. nil when -blast-radius is off
	// (the default) ⇒ unfenced, byte-identical. (The auto-approval $/count axes are moot here:
	// the swarm's promote gate uses a nil approver that never auto-lands, so there is nothing
	// to auto-approve.)
	blast := mintBlastBudget(*sf.common.blastRadius, d.log)
	newEnv := buildEnvFactory(buildDeps{
		runtime:     *sf.common.runtime,
		image:       *sf.common.image,
		sandboxPref: *sf.common.sandboxPref,
		blast:       blast,
	}, *sf.common.checkCmd)

	// --- the Integrator (FanInMerge / code preset only) or nil (FanInCollate). ---
	// The swarm threads each pass's PRIOR verified tip into the Integrator's BaseRef so
	// a pass folds its newly-green branches ONTO the prior tip rather than re-merging the
	// whole accumulated green set from base HEAD (MAJOR #6). buildIntegrateFunc's closure
	// is base-ref-less (it reads the Integrator's static BaseRef), so we wrap it in a
	// per-pass adapter that pins intr.BaseRef before each fold. Pinning is safe because
	// the Controller folds serially between passes (one fold per pass, never concurrent),
	// so no two folds race on the shared field. The integrator re-verifies every merge,
	// so the tip is ALWAYS verifier-green (I2).
	var integrateFn swarm.IntegrateFunc
	if pre.FanIn == preset.FanInMerge {
		intr := &integrate.Integrator{
			BaseRepo: repo,
			NewEnv:   func(dir string) integrate.Env { return integrate.Env{Verifier: newEnv(dir).Verifier} },
			Log:      d.log,
		}
		base := buildIntegrateFunc(intr)
		integrateFn = func(ctx context.Context, baseRef string, order []integrate.MergeItem) (string, []integrate.MergeResult, error) {
			intr.BaseRef = baseRef // "" ⇒ the integrator starts from HEAD (first pass)
			return base(ctx, order)
		}
	}

	// --- the shardFn I2 closure (the heart): write+verify each shard's typed artifact
	// and set Passed/Branch ONLY on a green verifier report. ---
	sc := &shardContext{
		deps:        d,
		preset:      pre,
		packName:    shardPack,
		deliverable: deliverables,
		egress:      shardEgress(pre, splitCSV(*sf.egressAllow)),
		pool:        pl,
		ledger:      ledger,
		board:       bd,
		repo:        repo,
		collateRoot: collateRoot,
		perShard:    *sf.perShard,
		newEnv:      newEnv,
	}
	runner := &swarm.Runner{Concurrency: *sf.concurrency, Fn: sc.run}

	policyPasses, err := passPolicyFromFlag(*sf.passes)
	if err != nil {
		return swarmAssembly{}, err
	}

	controller := &swarm.Controller{
		Runner:        runner,
		Queue:         queue,
		Worktree:      collateRoot,
		Policy:        policyPasses,
		Integrate:     integrateFn,
		Budget:        ledger,
		GlobalCeiling: *sf.budget, // the same wall SetGlobalCeiling got; read non-recordingly each pass
		Log:           d.log,
		// Emit a scoreboard_snapshot per pass so a replayed report reconstructs the
		// same scoreboard the live render showed (live == replay). Coalesced by
		// boardMinInterval; the forced final emit in run() guarantees the terminal one.
		OnPass: func() { bd.EmitSnapshot(d.log) },
	}

	// --- the initial shard set + the run state. A FRESH run shards the goal via the
	// preset's Sharder (List/Plan/Failure); a --resume run re-loads the prior run's
	// still-failed shards from the durable queue (requeue-only-failed) and threads its
	// persisted SwarmState (pass counter / retry ledger / tip) forward, so the resumed
	// run continues rather than re-deriving from scratch. ---
	var initial []swarm.Shard
	// Ledger.MaxAttempts arms the focused-requeue + conflict-rebuild budget: with
	// the historical zero, requeue was disabled entirely and "until-clean" was
	// effectively single-pass for red shards (a gap the DAG-completion work
	// surfaced). The --retries flag now sets it; 0 restores the old red-is-final.
	state := swarm.SwarmState{RunID: runID, Goal: *sf.goal, Preset: pre.Name,
		Ledger: requeue.Ledger{MaxAttempts: *sf.retries}}
	if resumed != nil {
		state = *resumed
		// Fix #13: `state = *resumed` adopts the PERSISTED Ledger.MaxAttempts wholesale, so
		// a `--resume --retries N` would silently ignore N. When --retries was EXPLICITLY
		// passed on THIS invocation, override the resumed budget with it (the operator asked
		// for a different retry ceiling on the resume); otherwise the persisted value stands.
		if d.explicitFlags["retries"] {
			state.Ledger.MaxAttempts = *sf.retries
		}
		// Gate eligibility on state.Ledger (which now carries any --retries override), not the
		// raw resumed one, so a widened budget actually re-admits shards the old ceiling had
		// exhausted — the whole point of `--resume --retries N`.
		failed, ferr := queue.Failed(context.Background(), &state.Ledger)
		if ferr != nil {
			return swarmAssembly{}, fmt.Errorf("swarm: resume: %w", ferr)
		}
		initial = failed
		// Fix #10: a shard the interrupted process durably Marked StatusPassed whose branch
		// was never folded (a crash between Mark and integrateGreen/SaveState, or a shard
		// mid-conflict-rebuild) is NOT in queue.Failed (wrong status) and NOT in st.Merged —
		// so a naive resume would leave its verified branch OFF the tip and exit 0 with the
		// work silently dropped (I2). Re-seed those passed-but-unmerged shards as queued so
		// the Controller re-runs and re-folds them (Release kept the branch; the Fn re-greens
		// and integrateGreen lands it). A passed shard already in st.Merged is skipped — it
		// is on the tip. resetForResume clears its terminal state so it dispatches afresh.
		unfolded, uerr := resumeUnfoldedPassed(context.Background(), queue, resumed)
		if uerr != nil {
			return swarmAssembly{}, fmt.Errorf("swarm: resume: %w", uerr)
		}
		initial = append(initial, unfolded...)
	} else {
		shards, serr := buildInitialShards(context.Background(), d, sf, pre, deliverables, shardPack, pl, runID)
		if serr != nil {
			return swarmAssembly{}, serr
		}
		initial = shards
	}
	for i := range initial {
		bd.MarkQueued(initial[i].ID)
	}
	bd.SetTotal(len(initial))

	// --- the single human gate for the final promote (nil approver default-denies).
	// Same evidence wiring as buildStack: the approver sees the promoted branch's
	// diffstat + bounded excerpt and the run's spend at the moment of decision. No
	// verify-tail recorder here — the swarm verifies per shard inside ShardFunc
	// closures, so there is no single "last report" to show; the section stays
	// empty and renderers skip it. ---
	swarmDiffer := func(branch string) (string, error) { return worktree.Diff(context.Background(), repo, branch) }
	gate := buildGateFuncEv(swarmApprover(), d.log, gateEvidenceFunc(swarmDiffer, nil, ledger))

	return swarmAssembly{
		controller:   controller,
		initial:      initial,
		state:        state,
		flags:        sf,
		ledger:       ledger,
		pool:         pl,
		board:        bd,
		preset:       pre,
		deliverables: deliverables,
		repo:         repo,
		collateRoot:  collateRoot,
		runID:        runID,
		logPath:      *sf.common.logPath,
		gate:         gate,
		style:        termui.New(os.Stdout).Style(),
		cleanup:      cleanup,
	}, nil
}

// run drives the multi-pass Controller, then emits the swarm_pass_clean signal on a
// converged run (the second leg of the report's FinalCleanPass gate) and offers the
// final clean tip as a single gated PromoteToBase candidate. It NEVER auto-lands: the
// gate's nil approver default-denies, so a converged run stops at the promote gate.
func (a swarmAssembly) run(ctx context.Context) (swarm.Outcome, error) {
	out, err := swarmViaKernel(ctx, a.controller, a.state, a.initial)
	// Always force the terminal scoreboard snapshot to the log (even on error/cap),
	// so a replayed report reflects the same final scoreboard the live render prints
	// — the per-pass OnPass emits are coalesced and may have skipped the last one.
	a.board.EmitSnapshotForce(a.log())
	if err != nil {
		return out, err
	}

	// Mark the live board clean and emit the swarm_pass_clean signal ONLY on a real
	// converge over a verified chain (I2/I5) — the report's FinalCleanPass requires
	// both an empty worklist AND a verified chain.
	chainOK := eventlogVerified(a.logPath)
	clean := out.Done && out.Remaining == 0 && chainOK
	a.board.MarkClean(out.Done && out.Remaining == 0, chainOK)
	if clean {
		a.log().Append(eventlog.Event{Kind: board.SwarmPassCleanKind, Detail: map[string]any{
			"run": a.runID, "passes": out.Passes,
		}})
		// Offer the converged tip to the single human gate. A nil approver
		// default-denies, so this NEVER auto-lands; it records the gate decision.
		if out.TipBranch != "" {
			// FIX 1: the gate + earned-trust boundary key on the merge TARGET base (the base
			// repo's current branch), NOT the source tip — GradedApprover.scopeFor reads
			// GateAction.Branch for both the trust bucket AND the "never auto-approve main/prod"
			// floor, so a tip there would let that structural floor go silent (a latent
			// auto-merge-into-main hazard). Resolve the base branch; the source tip rides in
			// Detail. A detached HEAD (no symbolic ref) leaves it empty ⇒ fall back to the tip.
			target := out.TipBranch
			if base, berr := baseBranchName(ctx, a.repo); berr == nil && base != "" {
				target = base
			}
			// GAA-T04: record the verifier-green promote boundary so graapprove.TrustView
			// can fold it into earned trust for promote-to-base on the TARGET base — the swarm
			// is a boundary_outcome SOURCE even though it never auto-lands itself. `passed` is
			// the verifier verdict (clean ⇐ out.Done && Remaining==0 && chainOK), never a
			// backend self-report (I2). Emitted before the gate so the audit order stays causal.
			emitBoundaryOutcome(a.log(), policy.PromoteToBase.String(), target, true)
			_ = a.gate(policy.GateAction{Type: policy.PromoteToBase, Branch: target,
				Detail: "promote converged swarm tip " + out.TipBranch + " → " + target})
		}
	}
	return out, nil
}

// renderReport renders the requested report (and a matrix when --artifact requested
// one) through the SAME report.go swarm path the `nilcore report` command uses, so
// live and replay share one renderer. It returns the rendered text and the exit code
// (1 when the chain failed to verify — fail-closed, regardless of converge).
func (a swarmAssembly) renderReport() (string, int) {
	st := a.style
	var sb strings.Builder
	exit := 0

	if a.deliverables.report {
		rendered, code, err := runSwarmReport(a.logPath, a.collateRoot, a.collateRoot, *a.flags.reportFmt, a.runID, *a.flags.out, st)
		if err == nil {
			sb.WriteString(rendered)
			if code != 0 {
				exit = code
			}
		}
	}
	// A matrix deliverable always renders the cross-shard pivot, regardless of the
	// per-shard Kind or the --report format (the headline `report+matrix` contract).
	if a.deliverables.matrix {
		rendered, code, err := runSwarmReport(a.logPath, a.collateRoot, a.collateRoot, "matrix", a.runID, "", st)
		if err == nil {
			sb.WriteString(rendered)
			if code != 0 {
				exit = code
			}
		}
	}
	return sb.String(), exit
}

// log returns the assembly's event log (the controller's).
func (a swarmAssembly) log() *eventlog.Log { return a.controller.Log }

// shardContext carries everything the shardFn closure needs for one shard. It is the
// per-run state the I2 closure reads; one instance is shared across every shard's
// invocation (the Runner calls run concurrently, but each call provisions its own
// worktree + box, so no shard touches another's Dir — I4).
type shardContext struct {
	deps        swarmDeps
	preset      preset.Preset
	packName    string
	deliverable deliverableSet
	egress      policy.Egress
	pool        *pool.Pool
	ledger      *budget.Ledger
	board       *board.Board
	repo        string
	collateRoot string
	perShard    float64 // per-shard dollar ceiling (0 = no cap); read from --per-shard-budget
	newEnv      func(dir string) buildEnv
}

// run is the shardFn (swarm.ShardFunc): it provisions a worktree off the repo, gets a
// box via the env factory, composes the governing verifier (packs.Build wrapped in
// swarm.NewShipGate — refusing verify.Pass/nil, I2), runs the worker (native via
// roster.NewWorker OR delegated via buildBackend in-box), then the verifier — NOT the
// worker self-report — governs Passed (I2). On green it copies the verified artifact
// into the shared collate root so requeue.Scan sees it and sets Branch; on red it
// preserves the failed attempt so a requeue can continue from it. It mirrors
// build.go's buildSpawnFunc.
func (c *shardContext) run(ctx context.Context, s swarm.Shard) spawn.Result {
	c.board.MarkRunning(s.ID)

	// Per-shard ceiling: the pool's per-task ledger ceiling (SetShardCeiling) when
	// --per-shard-budget is set. The scope is the pool's canonical shard scope.
	if c.perShard > 0 {
		c.pool.SetShardCeiling(s.ID, c.perShard)
	}

	// One worktree per ATTEMPT. The start-point honors the harness-resolved
	// Shard.BaseRef — a dependency's verified branch, the integrated tip (multi-dep /
	// conflict rebuild), or the shard's own preserved failed attempt (a focused
	// retry's continue_from) — so a DAG dependent actually SEES the work it depends
	// on instead of being cut blind from HEAD. Empty BaseRef ⇒ "HEAD", the prior
	// behavior. The branch carries a fresh per-attempt suffix because Release KEEPS
	// branches (that is what makes a preserved failed attempt continuable) and
	// `worktree add -b` refuses an existing branch — a fixed name would fail every
	// requeued attempt at the cut, and a retry must never advance the very ref its
	// own base was cut from. The shared gitMu (build.go) serializes the worktree-add
	// against concurrent shards.
	startPoint := s.BaseRef
	if startPoint == "" {
		startPoint = "HEAD"
	}
	branch := "swarm/" + leafName(s.ID)
	// Suffix the branch for any attempt that must NOT collide with an existing ref.
	// s.Attempt>0 / s.BaseRef!="" catches an in-process retry or rebase. But a
	// RESUME-LOADED pass-1 failure persists Attempt=0 and BaseRef="" (Fix #11), and its
	// unsuffixed branch swarm/<leaf> ALREADY EXISTS from the prior process (Release keeps
	// branches), so `git worktree add -b` would fail deterministically and the shard would
	// recordFail without ever running. So also suffix whenever the target branch already
	// exists — the branch check makes the resume-loaded case take a fresh, non-colliding ref.
	if s.Attempt > 0 || s.BaseRef != "" || swarmBranchExists(ctx, c.repo, branch) {
		branch += "-a" + shortID() // retry / rebased / resume-collision attempt: a fresh ref
	}
	gitMu.Lock()
	wt, err := worktree.CreateFrom(ctx, c.repo, branch, leafName(s.ID), startPoint)
	gitMu.Unlock()
	if err != nil {
		return c.recordFail(s, spawn.Result{ID: s.ID, State: spawn.StateFailed,
			Err: fmt.Errorf("swarm shard: worktree: %w", err)})
	}
	defer func() { gitMu.Lock(); _ = wt.Release(); gitMu.Unlock() }()

	env := c.newEnv(wt.Path())

	// No box ⇒ nothing to execute the worker in and nothing to re-run the per-claim
	// evidence checks against: fail the shard CLOSED (I2/I4). A nil box can only ever
	// resolve evidence claims Unverifiable, so a green shard is impossible here — we
	// refuse to run the worker (whose loop would panic on a nil sandbox) and record the
	// shard failed rather than silently shipping.
	if env.Box == nil {
		br := preserveFailedAttempt(ctx, wt)
		return c.recordFail(s, spawn.Result{ID: s.ID, Branch: br, Passed: false, State: spawn.StateFailed,
			Err: fmt.Errorf("swarm shard: no sandbox (fail-closed, I4)")})
	}

	// The per-shard governing verifier: packs.Build composes schema + the per-claim
	// in-box ArtifactVerifier (+ a raw build/browser child for code/ui) over the
	// artifact the worker writes at .nilcore/artifacts/<id>.json. ShipGate refuses a
	// vacuous verify.Pass/nil (I2). A nil box fails network claims closed, never Pass.
	//
	// The path is ABSOLUTE under the per-shard worktree root (env.Box.Workdir() ==
	// wt.Path()), mirroring the proven typed-research path (build.go): evverify reads
	// RelPath host-side via worktreefs.OpenNoFollow, so a CWD-relative path would look
	// under the nilcore process dir, not the worktree the worker actually wrote into —
	// every shard would read "artifact missing" and fail closed.
	//
	// This is PIN-and-verify, safe by construction: the harness AUTHORS the artifact id
	// (s.ID, a valid single-component id with no '/') in shardGoal and instructs the
	// worker to write exactly .nilcore/artifacts/<s.ID>.json — so the verifier checks the
	// one path the harness pinned, not a filename the worker chose. (The report-side
	// projection below is separately id-agnostic — it DISCOVERS the artifact the worker
	// wrote — because by then the run is over and the trust comes from the verified
	// status, not the path; the two stages are intentionally different.)
	relPath := filepath.Join(env.Box.Workdir(), ".nilcore", "artifacts", s.ID+".json")
	plan, perr := packs.Build(c.packName, env.Box, relPath, packs.DefaultSchemas())
	if perr != nil {
		return c.recordFail(s, spawn.Result{ID: s.ID, State: spawn.StateFailed,
			Err: fmt.Errorf("swarm shard: build verifier: %w", perr)})
	}
	gov, gerr := swarm.NewShipGate(plan.Verifier)
	if gerr != nil {
		return c.recordFail(s, spawn.Result{ID: s.ID, State: spawn.StateFailed,
			Err: fmt.Errorf("swarm shard: ship gate: %w", gerr)})
	}

	// Run the worker. A delegated coding backend (codex/claude-code) routes through
	// buildBackend IN-BOX (I4); native runs through roster.NewWorker. Both are judged
	// by the SAME governing verifier (gov) — the worker's self-report never ships (I2).
	// The backend's prose Summary is captured (bounded) so a PASSED shard can hand its
	// account to DAG dependents; it is model-authored prose the Controller's handoff
	// digest fences via guard.Wrap (I7) — never a verdict, never control text.
	var workerSummary string
	goal := c.shardGoal(s)
	task := backendTaskFor(s, goal, wt.Path())
	if backendName := c.pool.CodeBackendFor(s.Role); backendName != "native" {
		be := buildBackend(backendName, nil, c.deps.boot.cred, advisorCfg{}, env.Box, gov,
			c.deps.log, defaultShardMaxSteps, nil, c.repo, c.deps.boot.cfg)
		bres, rerr := be.Run(ctx, task)
		if rerr != nil {
			return c.recordFail(s, c.classifyWorkerErr(ctx, s, fmt.Errorf("swarm shard: delegated backend: %w", rerr)))
		}
		workerSummary = bres.Summary
	} else {
		prof := c.preset.Profile
		prof.Model = c.pool.WorkerFor(s.ID) // attach the LIVE worker provider (Resolve left it nil)
		// Apply the operator's unioned egress (preset hosts ∪ --egress-allow) to the
		// worker's profile via roster.EgressFor, which INTERSECTS the role's allowlist
		// with that tree (narrow-only — R9: it can only narrow within the tree, never
		// over-permit). Without this the --egress-allow flag was dead: worker egress came
		// solely from the preset profile (MAJOR #5). A deny-all role still intersects to
		// empty and keeps --network none.
		prof.Egress = roster.EgressFor(prof, c.egress)
		worker := roster.NewWorker(prof, env.Box, gov, c.deps.log, c.pool.WorkerFor(s.ID), nil)
		wres, rerr := worker.Run(ctx, task)
		if rerr != nil {
			return c.recordFail(s, c.classifyWorkerErr(ctx, s, fmt.Errorf("swarm shard: worker: %w", rerr)))
		}
		workerSummary = wres.Summary
	}

	// The verifier — not the worker self-report — decides whether this shard ships (I2).
	rep, verr := gov.Check(ctx)

	// Copy the VERDICT-OVERWRITTEN artifact (evverify.Check wrote the real per-claim
	// statuses back to the worktree file) into the shared collate root REGARDLESS of
	// pass/fail, so requeue.Scan — which reads <collateRoot>/.nilcore/artifacts/*.json —
	// sees THIS shard's claims. A RED artifact MUST be visible for the Controller to
	// requeue it (and to count it as remaining); copying only green artifacts would make
	// a fully-failed run falsely converge (Scan finds nothing red ⇒ empty worklist ⇒
	// done). Best-effort: a shard whose worker wrote no parseable artifact contributes
	// nothing and is treated as red below (the verifier already failed it).
	collateArtifact(c.collateRoot, wt.Path(), s.ID)

	if verr != nil || !rep.Passed {
		// Preserve the unverified attempt so a requeue can continue_from it.
		br := preserveFailedAttempt(ctx, wt)
		res := spawn.Result{ID: s.ID, Branch: br, Passed: false, State: spawn.StateFailed}
		if verr != nil {
			res.Err = fmt.Errorf("swarm shard: verify: %w", verr)
		}
		// A verify-failed shard still wrote a verdict-overwritten artifact; record the
		// governing RED claim's trusted provenance from it (recordFail's nil path is for
		// early backend/setup fails that produced no artifact).
		c.recordVerdict(s, false, readTrustedArtifact(wt.Path()))
		return res
	}

	// Green: the artifact is already collated above. On a code preset, commit the
	// worktree and surface the branch for the Integrator. The bounded prose Summary
	// rides along for the DAG handoff digest (fenced downstream — I7).
	res := spawn.Result{ID: s.ID, Passed: true, State: spawn.StatePassed,
		Summary: truncate(workerSummary, maxReportProseBytes)}
	if c.preset.FanIn == preset.FanInMerge {
		gitMu.Lock()
		_, _, cerr := wt.Commit(ctx, "feat("+leafName(s.ID)+"): "+truncate(goal, 60))
		gitMu.Unlock()
		// A commit FAULT on a verifier-green code shard is a hard shard FAILURE, never a
		// silent drop: discarding it (the prior `if cerr == nil` guard) left res.Branch
		// empty while res.Passed stayed true, so unmergedGreens/integrateGreen skipped the
		// branchless green and requeue.Scan found nothing red — the run reported Done while
		// the committed code never reached integration (a false-green, I2). commitFaultIsFatal
		// encodes the decision: a fault fails the shard; a clean tree (changed=false,
		// cerr==nil) is NOT a fault (Commit returns the HEAD sha and the branch already
		// points at the verified tree), so the branch is surfaced normally.
		if commitFaultIsFatal(cerr) {
			br := preserveFailedAttempt(ctx, wt)
			return c.recordFail(s, spawn.Result{ID: s.ID, Branch: br, Passed: false, State: spawn.StateFailed,
				Err: fmt.Errorf("swarm shard: commit green worktree: %w", cerr)})
		}
		res.Branch = branch
	}
	// Project the trusted (harness-set) artifact fields onto the Result for the report.
	// readVerifiedArtifact is id-agnostic (P11 #48): it discovers the single artifact the
	// worker wrote under this worktree's .nilcore/artifacts/ rather than assuming a
	// filename — which is exactly right here (one artifact per shard worktree).
	if as := readVerifiedArtifact(wt.Path()); as != nil {
		res.Artifact = as
	}
	// recordVerdict needs the full artifact (verifier-set Verifier/SourceURL for the
	// board's trusted provenance), which the flat res.Artifact summary drops — re-read it.
	c.recordVerdict(s, true, readTrustedArtifact(wt.Path()))
	return res
}

// commitFaultIsFatal reports whether a wt.Commit outcome on a verifier-green code
// shard must FAIL the shard. A non-nil error is a genuine git fault (a failed
// stage/commit) and is fatal — a green shard whose commit faulted must never
// surface a branchless green (which unmergedGreens/integrateGreen skip and
// requeue.Scan can't see, producing a false Done — I2). A nil error is NOT fatal,
// including the clean-tree case (Commit returns changed=false with the HEAD sha):
// the shard branch already points at the verified tree, so it merges harmlessly.
// Isolated so the false-green regression is unit-tested without a sandbox.
func commitFaultIsFatal(commitErr error) bool { return commitErr != nil }

// shardGoal frames the shard's task with the typed-artifact instruction: the worker
// MUST write its claims to .nilcore/artifacts/<id>.json (the out-of-band path the
// verifier reads). The shard's own Goal carries the model-facing work; the artifact
// instruction is harness-authored control text (the shard Input/Goal is DATA — I7 is
// enforced at the worker boundary).
func (c *shardContext) shardGoal(s swarm.Shard) string {
	return fmt.Sprintf(
		"%s\n\nWrite your findings as a typed %s artifact JSON to .nilcore/artifacts/%s.json "+
			"with id %q. Each claim must carry an Evidence.SourceURL (key-free) the verifier can "+
			"re-check; the verifier — not your self-report — decides whether this shard ships.",
		s.Goal, c.deliverable.kind, s.ID, s.ID)
}

// classifyWorkerErr builds the failed Result for a worker/backend error, running the
// error through swarm.ClassifyCeiling so a budget.ErrCeiling caught at the SHARD
// boundary is attributed to the per-shard ceiling vs the global wall. This is the
// single mechanism SWARM.md names for that decision: the global rail stays owned by the
// Controller's non-recording globalBudgetExhausted probe (it stops the whole run at the
// next pass top), so here we only ANNOTATE the shard's failure with the classified
// scope — a per-shard exhaustion is the shard's own ceiling (fail this shard, the run
// continues), a global exhaustion is the wall (this shard fails; the Controller stops
// the run). A non-ceiling error classifies to ScopeNone and rides through unchanged.
// The scope key is the pool's canonical per-shard ledger scope so the probe charges the
// exact key SetShardCeiling armed.
func (c *shardContext) classifyWorkerErr(ctx context.Context, s swarm.Shard, err error) spawn.Result {
	res := spawn.Result{ID: s.ID, State: spawn.StateFailed, Err: err}
	switch swarm.ClassifyCeiling(ctx, c.ledger, c.pool.Scope(s.ID), err) {
	case swarm.ScopeShard:
		res.Err = fmt.Errorf("per-shard budget exhausted: %w", err)
	case swarm.ScopeGlobal:
		res.Err = fmt.Errorf("global budget ceiling exhausted (run will stop): %w", err)
	}
	return res
}

// recordFail folds a failed shard verdict into the board and returns the Result. An
// early fail (backend/setup error) has no verdict-overwritten artifact to project, so it
// records the fail with a nil trusted projection; the artifact-bearing red path
// (verify failure) reads the full artifact from the worktree and calls recordVerdict
// directly (see the shard closure) so the governing red claim's provenance surfaces.
func (c *shardContext) recordFail(s swarm.Shard, res spawn.Result) spawn.Result {
	c.recordVerdict(s, false, nil)
	return res
}

// readTrustedArtifact re-reads the single verdict-overwritten artifact from a shard's
// worktree as a full *artifact.Artifact, for the I7-safe swarm.ProjectTrusted projection
// (which needs the verifier-set Verifier/SourceURL that the flat spawn.ArtifactSummary
// deliberately drops). Fail-closed: no / unparseable artifact yields nil, and recordVerdict
// then leaves the board row's provenance unset. The read goes through artifact.Read
// (worktreefs O_NOFOLLOW), so a symlink at the target is refused rather than followed (I4).
func readTrustedArtifact(root string) *artifact.Artifact {
	paths := artifactFiles(root)
	if len(paths) == 0 {
		return nil
	}
	a, err := artifact.Read(root, artifactID(paths[0]))
	if err != nil {
		return nil
	}
	return a
}

// recordVerdict feeds the board the verifier verdict for one shard (the ONLY input
// that moves the pass/fail tally — I2), keyed by the shard's pass (Attempt+1). When an
// artifact is present it is run through swarm.ProjectTrusted — the I7 guard that carries
// ONLY the verifier-set Status/Verifier and the key-free SourceURL, NEVER the
// model-authored Value/Statement — and the governing claim's trusted provenance is
// surfaced on the board's trace projection. Using ProjectTrusted (rather than reading
// claim fields ad hoc here) is what makes the "no model Value reaches the scoreboard"
// property structural: the projection has no field to hold a Value.
func (c *shardContext) recordVerdict(s swarm.Shard, passed bool, a *artifact.Artifact) {
	o := board.ShardOutcome{ID: s.ID, Pass: s.Attempt + 1, Passed: passed}
	if gc, ok := swarm.GoverningTrustedClaim(swarm.ProjectTrusted(a)); ok {
		o.Verifier = gc.Verifier
		o.Status = string(gc.Status)
		o.SourceURL = gc.SourceURL
	}
	c.board.Record(o)
}

// collateArtifact copies the verdict-overwritten artifact for shard `id` from its
// per-shard worktree into the shared collate root, so requeue.Scan (which reads
// <collateRoot>/.nilcore/artifacts/*.json) sees this shard's claims REGARDLESS of
// pass/fail. A RED artifact must be collated too — copying only green ones would make a
// fully-failed run falsely converge (Scan finds nothing red ⇒ empty worklist ⇒ done).
// Best-effort: a shard whose worker wrote no parseable artifact contributes nothing
// (the verifier already failed it). `id` MUST be a valid single-component artifact id
// (no '/'); a '/'-containing id makes artifact.Read/Write silently no-op — exactly the
// false-convergence blocker — so the sharder mints '-'-delimited ids that satisfy this.
func collateArtifact(collateRoot, wtPath, id string) {
	if a, err := artifact.Read(wtPath, id); err == nil && a != nil {
		_ = artifact.Write(collateRoot, a)
	}
}

// ---------------------------------------------------------------------------
// resolution helpers
// ---------------------------------------------------------------------------

// buildInitialShards maps the preset's SharderKind onto a concrete swarm.Sharder and
// produces the initial shard set. SharderList expands a --shard-file (or the goal as a
// single line); SharderPlan asks the pool's planner to decompose the goal; SharderFailure
// derives one shard per red test from a box over the repo. Every sharder carries the
// preset's Kind/Pack/Role/Tier as plain routing fields (preset never imports swarm).
func buildInitialShards(ctx context.Context, d swarmDeps, sf swarmFlags, pre preset.Preset, del deliverableSet, packName string, pl *pool.Pool, runID string) ([]swarm.Shard, error) {
	role := string(pre.Role)
	goal := *sf.goal
	// Pre-built plan (e.g. `nilcore flows run`): the caller already knows the DAG, so
	// shard it via TreeSharder (no planner model call) — carrying the preset's routing
	// fields exactly as the goal-based sharders do. This routes the flow's DependsOn edges
	// onto real Shard.Deps so the runner honors the ordering instead of a flat goal list.
	if d.tree != nil {
		return swarm.TreeSharder{Tree: *d.tree, Kind: del.kind, Pack: packName, Role: role, Tier: pre.WorkerTier}.Shards(ctx, goal, runID)
	}
	var sh swarm.Sharder
	switch pre.Sharder {
	case preset.SharderList:
		lines, err := shardLines(*sf.shardFile, goal)
		if err != nil {
			return nil, err
		}
		sh = swarm.ListSharder{Lines: lines, Kind: del.kind, Pack: packName, Role: role, Tier: pre.WorkerTier}
	case preset.SharderPlan:
		// --agents is a soft target the planner is asked to aim for (the PlanSharder
		// carries no hard count — the plan decides the work); when set it is woven into
		// the goal as a planning hint, so it shapes the decomposition without overriding
		// the plan's own dependency structure.
		if *sf.agents > 0 {
			goal = fmt.Sprintf("%s\n\nAim for roughly %d independent units of work.", goal, *sf.agents)
		}
		sh = swarm.PlanSharder{Model: pl.Planner(), Kind: del.kind, Pack: packName, Role: role, Tier: pre.WorkerTier}
	case preset.SharderFailure:
		box := selectSandbox(*sf.common.sandboxPref, *sf.common.runtime, *sf.common.image, d.dir)
		sh = swarm.FailureSharder{Box: box, Kind: del.kind, Pack: packName, Role: role, Tier: pre.WorkerTier}
	default:
		return nil, fmt.Errorf("swarm: preset %q has an unknown sharder %q", pre.Name, pre.Sharder)
	}
	return sh.Shards(ctx, goal, runID)
}

// shardLines reads the operator shard list: a --shard-file (one unit per line) when
// given, else the goal as a single line. A missing/unreadable shard-file is an error.
func shardLines(path, goal string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		if strings.TrimSpace(goal) == "" {
			return nil, fmt.Errorf("swarm: the list sharder needs --shard-file or --goal")
		}
		return []string{goal}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("swarm: read shard file: %w", err)
	}
	return strings.Split(string(data), "\n"), nil
}

// poolConfigFromFlags assembles a pool.PoolConfig from the swarm flags layered over
// any onboard.Config.Pool the operator persisted (the flags override the config). It
// is pure data: every field is a "provider:model" spec, a duration string, or a count —
// NEVER a key (I3; keys resolve by name through cred at pool.Build time).
func poolConfigFromFlags(sf swarmFlags, b boot) pool.PoolConfig {
	cfg := pool.PoolConfig{}
	if b.cfg.Pool != nil {
		cfg = *b.cfg.Pool
	}
	if *sf.workerModel != "" {
		cfg.Worker.Spec = *sf.workerModel
	}
	if *sf.plannerModel != "" {
		cfg.Planner.Spec = *sf.plannerModel
	}
	if *sf.verifyModel != "" {
		cfg.Verifier.Spec = *sf.verifyModel
	}
	if fb := *sf.fallbackModel; fb != "" {
		cfg.Worker.Fallback = fb
		cfg.Planner.Fallback = fb
		cfg.Verifier.Fallback = fb
	}
	if cb := *sf.codeBackend; cb != "" && cb != "native" {
		cfg.Worker.CodeBackend = cb
	}
	cfg.Jitter = sf.jitter.String()
	if cfg.Caps == nil && len(sf.providerCap) > 0 {
		cfg.Caps = map[string]int{}
	}
	for k, v := range sf.providerCap {
		cfg.Caps[k] = v
	}
	return cfg
}

// shardEgress widens the preset's derived egress (the union of its verify-pack hosts)
// with any operator --egress-allow hosts. It NEVER narrows — the per-role EgressFor
// intersection still governs at the worker boundary (R9). An empty result keeps every
// shard at --network none.
func shardEgress(pre preset.Preset, extra []string) policy.Egress {
	allowed := append([]string{}, pre.Egress...)
	allowed = append(allowed, extra...)
	return policy.Egress{Allowed: allowed}
}

// validatePacks asserts every named pack is known by attempting a Select into a
// throwaway registry — the SAME fail-closed check packs.Build runs per shard. An
// unknown pack is FATAL at startup (fail-closed), never a vacuous default.
func validatePacks(names []string) error {
	for _, n := range names {
		if _, err := packs.Build(n, nil, ".nilcore/artifacts/probe.json", packs.DefaultSchemas()); err != nil {
			return fmt.Errorf("swarm: --verify-pack: %w", err)
		}
	}
	return nil
}

// backendTaskFor renders a shard as a backend.Task. The harness owns sandbox/egress/
// tools via NewWorker/buildBackend; the shard contributes only id/goal/dir, so a shard
// can never widen the worker's authority (I1/I7).
func backendTaskFor(s swarm.Shard, goal, dir string) backend.Task {
	return backend.Task{ID: s.ID, Goal: goal, Dir: dir}
}

// openSwarmStore opens the persistent store the durable queue persists shard rows to,
// wired as the event log's second backing (mirroring setupPersistence). The durable
// queue's resume contract requires a real store, so a missing data dir / open failure
// is FATAL here (unlike the best-effort run-path persistence). cleanup closes the
// store handle.
func openSwarmStore(log *eventlog.Log) (*store.Store, func()) {
	dir, err := paths.EnsureDir(paths.DataDir())
	if err != nil {
		fatal(fmt.Errorf("swarm: data dir: %w", err))
	}
	s, err := store.Open(filepath.Join(dir, "nilcore.db"))
	if err != nil {
		fatal(fmt.Errorf("swarm: open store: %w", err))
	}
	log.UseStore(s)
	return s, func() { _ = s.Close() }
}

// swarmApprover is the human approver for the final promote gate. The swarm NEVER
// auto-lands: a nil approver default-denies (no ambient authority for an irreversible
// step, I3). buildGateFunc routes the structured PromoteToBase action through it.
func swarmApprover() policy.Approver { return nil }

// loadResumeState discovers the most recent interrupted swarm run in the durable store
// and returns its persisted SwarmState (the run id, pass counter, retry ledger, and
// integration tip the resume continues from), or (nil, nil) when no in-flight run
// exists. It is LOCAL-process-restart resume over the LOCAL store only (never cross-
// host). A store fault is surfaced (fail-loud), never swallowed into a silent fresh run.
func loadResumeState(ctx context.Context, st *store.Store, log *eventlog.Log) (*swarm.SwarmState, error) {
	// A queue bound to an empty run id can still enumerate every run's in-flight row
	// (InFlightSwarm filters on the run status, not the id), so we discover the runs and
	// then read the most recent one's state through a queue bound to ITS id.
	probe := swarm.NewQueue(st, log, "")
	rows, err := probe.InFlightSwarm(ctx)
	if err != nil {
		return nil, fmt.Errorf("swarm: resume: list in-flight: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	// The last row is the most recently written run (store rows are id-ordered, and a
	// run id embeds a monotonic suffix). Read its state through a correctly-bound queue.
	runRow := rows[len(rows)-1]
	// The run row id is "swarm-<runID>" (the Queue's runRowID); recover the runID by
	// stripping that exact prefix so the rebound queue's runRowID() round-trips back to
	// this same row. (Must match queue.runRowID — a '/' here would mis-bind the queue and
	// LoadState would miss the row.)
	runID := strings.TrimPrefix(runRow.ID, "swarm-")
	rq := swarm.NewQueue(st, log, runID)
	state, err := rq.LoadState(ctx)
	if err != nil {
		return nil, fmt.Errorf("swarm: resume: load state for %q: %w", runID, err)
	}
	return &state, nil
}

// resumeUnfoldedPassed returns the run's durably-PASSED shards whose verified branch
// never reached the integration tip (their id is NOT in the persisted SwarmState.Merged
// set) — the shards a crash between Mark(passed) and integrateGreen/SaveState, or a
// shard interrupted mid-conflict-rebuild, left stranded. queue.Failed does NOT return
// them (they are in the swarm-passed status namespace, not swarm-failed), so without
// this a resume would silently drop their verified work and exit 0 (I2, Fix #10). Each
// is reset to a queued, first-attempt shard so the Controller re-runs and re-folds it
// (Release kept the branch; the Fn re-greens and integrateGreen lands it). A nil/absent
// resumed state means a fresh run — nothing to re-seed.
func resumeUnfoldedPassed(ctx context.Context, q *swarm.Queue, resumed *swarm.SwarmState) ([]swarm.Shard, error) {
	if resumed == nil {
		return nil, nil
	}
	merged := make(map[string]bool, len(resumed.Merged))
	for _, id := range resumed.Merged {
		merged[id] = true
	}
	all, err := q.ShardsByRun(ctx)
	if err != nil {
		return nil, fmt.Errorf("resume: list shards: %w", err)
	}
	var out []swarm.Shard
	for _, s := range all {
		if s.State != swarm.ShardPassed || merged[s.ID] {
			continue // not passed, or already folded onto the tip — nothing to re-fold
		}
		out = append(out, resetForResume(s))
	}
	return out, nil
}

// resetForResume re-arms a durably-passed-but-unfolded shard as a fresh queued,
// first-attempt unit so the Controller dispatches it again. It clears the preserved
// branch and base so the re-run cuts cleanly (the Fn mints a new branch on green) rather
// than inheriting a stale ref, and zeroes Attempt so the retry budget starts fresh.
func resetForResume(s swarm.Shard) swarm.Shard {
	s.State = swarm.ShardQueued
	s.Branch = ""
	s.BaseRef = ""
	s.Attempt = 0
	return s
}

// boardMinInterval coalesces scoreboard_snapshot emits so a hot inner loop cannot
// flood the log; the live==replay contract still holds (the LAST snapshot is final).
const boardMinInterval = 250 * time.Millisecond

// defaultShardMaxSteps bounds each shard worker's tool-call budget. It mirrors the
// run path's native default so a shard worker is neither starved nor unbounded.
const defaultShardMaxSteps = 60

// swarmRunID mints a short, process-unique run id for the swarm's shard/queue
// namespace ("swarm/<runID>/<n>"). It reuses shortID's monotonic+nanosecond source.
func swarmRunID() string { return "run-" + shortID() }

// swarmBranchExists reports whether `branch` already exists in repo, via a hardened
// `git rev-parse --verify` (I4 clamp, shared with the rest of cmd's host-side git). It
// is used to decide whether a shard's target branch must take a fresh suffix: Release
// keeps branches, so a resume-loaded pass-1 failure (persisted Attempt=0, BaseRef="")
// would otherwise collide with its own prior unsuffixed branch and fail the worktree
// add (Fix #11). A git error (missing ref, or any fault) is treated as "does not
// exist" — the worktree-add then either succeeds on the clean name or fails loudly with
// its own error, so a false negative never silently loses the shard.
func swarmBranchExists(ctx context.Context, repo, branch string) bool {
	if strings.TrimSpace(branch) == "" {
		return false
	}
	// refs/heads/<branch> pins the lookup to a LOCAL branch, never a tag/remote of the
	// same name, so the existence test matches exactly what `worktree add -b` would clash on.
	err := hardenedGit(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run()
	return err == nil
}

// eventlogVerified reports whether the append-only log's hash chain verifies. A
// broken chain forces the run RED (I5) — a swarm must not read green over a tampered
// trail. A read error is treated as unverified (fail-closed).
func eventlogVerified(path string) bool { return eventlog.Verify(path) == nil }

// splitCSV splits a comma-separated flag value into trimmed, non-empty tokens.
func splitCSV(v string) []string {
	var out []string
	for _, raw := range strings.Split(v, ",") {
		if t := strings.TrimSpace(raw); t != "" {
			out = append(out, t)
		}
	}
	return out
}
