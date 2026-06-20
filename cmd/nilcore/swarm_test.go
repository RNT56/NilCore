package main

// swarm_test.go is the hermetic test of the `nilcore swarm` wiring (SW-T17). It
// drives NO container and NO network: it injects a temp git repo, a shared
// budget.Ledger, and a pool built over a fake-keyed cred resolver (provider objects
// are constructed but never called for the resolution-only assertions). It proves
// the load-bearing properties of the wiring:
//
//   - the §8.2 flags parse with the documented defaults;
//   - an unknown --preset / --verify-pack is FATAL at startup (fail-closed);
//   - ONE shared budget.Ledger backs every metered tier (a pre-exhausted ledger
//     leaves the pool's per-shard AND global headroom non-positive);
//   - the --artifact consumer parses report+matrix into BOTH a report and a matrix;
//   - the per-shard verifier is the packs.Build composite (never verify.Pass), and
//     a ui-preset shard with a nil box fails closed (Passed:false);
//   - --code-backend codex routes a shard through buildBackend, not roster.NewWorker;
//   - no existing package imports internal/swarm*/internal/pool, and the new leaves
//     have no global-side-effect init().
//
// The full run (real spawn/verify over a container) is deliberately NOT exercised —
// it needs a sandbox — so the wiring is validated at its seams.

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/packs"
	"nilcore/internal/budget"
	"nilcore/internal/eventlog"
	"nilcore/internal/meter"
	"nilcore/internal/pool"
	"nilcore/internal/requeue"
	"nilcore/internal/store"
	"nilcore/internal/swarm"
	"nilcore/internal/swarm/board"
	"nilcore/internal/swarm/preset"
	"nilcore/internal/verify"
)

// fakeCred returns a non-empty key for ANTHROPIC_API_KEY so pool.Build can construct
// the Anthropic provider object (no network — construction only). Every other lookup
// is empty.
func fakeCred(env string) string {
	if env == "ANTHROPIC_API_KEY" {
		return "test-key"
	}
	return ""
}

// testBoot is a boot with the fake cred resolver and an empty config.
func testBoot() boot { return boot{cred: fakeCred} }

// parseSwarmFlags parses args into a swarmFlags for the default-parse + deliverable
// tests without dispatching swarmMain.
func parseSwarmFlags(t *testing.T, args ...string) swarmFlags {
	t.Helper()
	fs := flag.NewFlagSet("swarm", flag.ContinueOnError)
	sf := registerSwarmFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return sf
}

func TestSwarmDefaultFlags(t *testing.T) {
	sf := parseSwarmFlags(t)
	if *sf.preset != "research" {
		t.Errorf("default preset = %q, want research", *sf.preset)
	}
	if *sf.concurrency != 1 {
		t.Errorf("default concurrency = %d, want 1", *sf.concurrency)
	}
	if *sf.passes != "until-clean" {
		t.Errorf("default passes = %q, want until-clean", *sf.passes)
	}
	if *sf.budget != 25.00 {
		t.Errorf("default budget = %v, want 25.00", *sf.budget)
	}
	if *sf.jitter != 750*time.Millisecond {
		t.Errorf("default jitter = %v, want 750ms", *sf.jitter)
	}
	if *sf.reportFmt != "text" {
		t.Errorf("default report = %q, want text", *sf.reportFmt)
	}
	if *sf.codeBackend != "native" {
		t.Errorf("default code-backend = %q, want native", *sf.codeBackend)
	}
	// The pass policy parses to until-clean.
	pp, err := passPolicyFromFlag(*sf.passes)
	if err != nil || !pp.UntilClean {
		t.Fatalf("passPolicyFromFlag(until-clean) = %+v, %v; want UntilClean", pp, err)
	}
	// A bounded N parses to MaxPasses; an invalid value fails closed.
	if pp, err := passPolicyFromFlag("3"); err != nil || pp.MaxPasses != 3 || pp.UntilClean {
		t.Fatalf("passPolicyFromFlag(3) = %+v, %v; want MaxPasses=3", pp, err)
	}
	if _, err := passPolicyFromFlag("garbage"); err == nil {
		t.Errorf("passPolicyFromFlag(garbage) should fail closed")
	}
}

func TestSwarmUnknownPresetIsError(t *testing.T) {
	sf := parseSwarmFlags(t, "-preset", "does-not-exist", "-goal", "x")
	_, err := buildSwarm(swarmDeps{flags: sf, boot: testBoot(), log: discardLog(t), dir: t.TempDir(),
		ledger: budget.New(), pool: testPool(t), store: testStore(t)})
	if err == nil {
		t.Fatal("unknown preset should be an error (fail-closed)")
	}
	if !errorsContains(err, "unknown preset") {
		t.Errorf("error = %v, want it to name the unknown preset", err)
	}
}

func TestSwarmUnknownVerifyPackIsError(t *testing.T) {
	sf := parseSwarmFlags(t, "-preset", "research", "-verify-pack", "nope-not-a-pack", "-goal", "x")
	_, err := buildSwarm(swarmDeps{flags: sf, boot: testBoot(), log: discardLog(t), dir: t.TempDir(),
		ledger: budget.New(), pool: testPool(t), store: testStore(t)})
	if err == nil {
		t.Fatal("unknown verify-pack should be an error (fail-closed)")
	}
	if !errorsContains(err, "verify-pack") && !errorsContains(err, "unknown pack") {
		t.Errorf("error = %v, want it to name the unknown pack", err)
	}
}

// TestSwarmSharedLedger proves a SINGLE shared ledger backs every metered tier: a
// pre-exhausted ledger handed to buildSwarm leaves the pool's per-shard scope AND the
// global scope with non-positive headroom — i.e. all metered providers read the same
// wall. (Headroom is the non-charging probe the controller runs; it needs no network.)
func TestSwarmSharedLedger(t *testing.T) {
	led := budget.New()
	led.SetGlobalCeiling(1.00)

	// The benchmark preset uses the ListSharder (no model call), so buildSwarm assembles
	// the stack hermetically — no planner round-trip. A shard file supplies the units.
	shardFile := filepath.Join(t.TempDir(), "shards.txt")
	if err := writeFile(shardFile, "benchA\nbenchB\n"); err != nil {
		t.Fatal(err)
	}
	sf := parseSwarmFlags(t, "-preset", "benchmark", "-shard-file", shardFile, "-budget", "1.00")
	// Build a real pool over this exact ledger (the production path), so the pool's
	// SetGlobalCeiling + WorkerFor scopes all charge THIS ledger.
	pl, err := pool.Build(pool.PoolConfig{}, led, fakeCred, "test-run", pool.Options{})
	if err != nil {
		t.Fatalf("pool.Build: %v", err)
	}
	asm, err := buildSwarm(swarmDeps{flags: sf, boot: testBoot(), log: discardLog(t), dir: newGoRepo(t),
		ledger: led, pool: pl, store: testStore(t)})
	if err != nil {
		t.Fatalf("buildSwarm: %v", err)
	}
	if asm.ledger != led {
		t.Fatal("assembly does not carry the injected ledger — not one shared wall")
	}

	// Exhaust the global ceiling by charging UP TO it through the shared ledger (a
	// charge that lands exactly on the ceiling is recorded; one that exceeds it is
	// refused and records nothing). After this the shared ledger has zero headroom.
	ctx := context.Background()
	if err := led.Charge(ctx, "swarm/test-run/0", 0, 1.00); err != nil {
		t.Fatalf("charge to the ceiling: %v", err)
	}

	// The pool reads the SAME ledger: global headroom is now non-positive, and so is a
	// shard scope's (both clamp to the binding global wall).
	if h, _ := asm.pool.Headroom(ctx, asm.pool.Scope("swarm/test-run/0")); h > 0 {
		t.Errorf("shard headroom = %v after exhaustion, want <= 0 (shared wall)", h)
	}
	if h, _ := asm.pool.Headroom(ctx, "swarm/test-run/planner"); h > 0 {
		t.Errorf("global headroom = %v after exhaustion, want <= 0 (shared wall)", h)
	}
}

// TestSwarmArtifactReportMatrix proves the --artifact consumer parses "report+matrix"
// into BOTH a report deliverable and a matrix deliverable (the headline contract).
func TestSwarmArtifactReportMatrix(t *testing.T) {
	d, err := parseDeliverables("report+matrix", artifact.KindDossier)
	if err != nil {
		t.Fatalf("parseDeliverables: %v", err)
	}
	if !d.report {
		t.Error("report+matrix should produce a report deliverable")
	}
	if !d.matrix {
		t.Error("report+matrix should produce a matrix deliverable")
	}
	if d.kind != artifact.KindReport {
		t.Errorf("report+matrix per-shard kind = %q, want report", d.kind)
	}

	// An empty list defaults to the preset Kind + a single report (no matrix).
	def, err := parseDeliverables("", artifact.KindDossier)
	if err != nil {
		t.Fatalf("parseDeliverables(empty): %v", err)
	}
	if !def.report || def.matrix || def.kind != artifact.KindDossier {
		t.Errorf("empty deliverable = %+v, want {report, no matrix, dossier}", def)
	}

	// matrix ALONE still triggers the cross-shard pivot, leaving the per-shard Kind at
	// the preset default (a matrix is a pivot, not a per-shard Kind).
	mo, err := parseDeliverables("matrix", artifact.KindBenchmark)
	if err != nil {
		t.Fatalf("parseDeliverables(matrix): %v", err)
	}
	if !mo.matrix || mo.kind != artifact.KindBenchmark {
		t.Errorf("matrix-only = %+v, want {matrix, kind=benchmark}", mo)
	}

	// An unknown deliverable token fails loudly.
	if _, err := parseDeliverables("report+bogus", artifact.KindReport); err == nil {
		t.Error("unknown --artifact token should be an error")
	}
}

// TestSwarmPerShardVerifierIsArtifactOnlyForCollate asserts the per-shard verifier the
// shardFn builds for a COLLATE preset (research → web/finance) is the packs.Build
// composite (schema + per-claim evidence), NEVER a verify.Pass, and that the ship gate
// refuses a vacuous verifier.
func TestSwarmPerShardVerifierIsArtifactOnlyForCollate(t *testing.T) {
	// The ship gate refuses verify.Pass{} (the I2 construction guard).
	if _, err := swarm.NewShipGate(verify.Pass{}); err == nil {
		t.Fatal("NewShipGate(verify.Pass{}) must be refused (I2)")
	}
	if _, err := swarm.NewShipGate(nil); err == nil {
		t.Fatal("NewShipGate(nil) must be refused (I2)")
	}

	// The research preset's primary pack composes a real verifier (not verify.Pass).
	pre, _, err := preset.Resolve("research")
	if err != nil {
		t.Fatalf("resolve research: %v", err)
	}
	plan := mustBuildPack(t, pre.VerifyPacks[0])
	if _, isPass := plan.Verifier.(verify.Pass); isPass {
		t.Fatal("collate preset verifier must not be verify.Pass (I2)")
	}
	if _, err := swarm.NewShipGate(plan.Verifier); err != nil {
		t.Errorf("ship gate should accept the composed collate verifier: %v", err)
	}

	// The code preset ANDs a raw build child into the composite when a box is present
	// (vs. the schema+evidence-only collate verifier). We assert the code pack composes
	// at least as much as the collate one — distinct, non-vacuous, accepted by the gate.
	codePre, _, err := preset.Resolve("code")
	if err != nil {
		t.Fatalf("resolve code: %v", err)
	}
	codePlan := mustBuildPack(t, codePre.VerifyPacks[0])
	if _, isPass := codePlan.Verifier.(verify.Pass); isPass {
		t.Fatal("code preset verifier must not be verify.Pass (I2)")
	}
}

// TestSwarmUIPresetNilBoxFailsClosed runs the shardFn for a ui-preset shard with a nil
// box and asserts the verdict is Passed:false — a shard whose evidence cannot be
// checked (no box) NEVER ships green (I2/I4 fail-closed).
func TestSwarmUIPresetNilBoxFailsClosed(t *testing.T) {
	pre, _, err := preset.Resolve("ui")
	if err != nil {
		t.Fatalf("resolve ui: %v", err)
	}
	repo := newGoRepo(t)
	sc := &shardContext{
		deps:        swarmDeps{boot: testBoot(), log: discardLog(t)},
		preset:      pre,
		packName:    pre.VerifyPacks[0],
		deliverable: deliverableSet{kind: pre.Kind},
		pool:        testPool(t),
		ledger:      budget.New(),
		board:       newTestBoard(),
		repo:        repo,
		collateRoot: repo,
		// newEnv returns a buildEnv with a NIL box: no sandbox to verify the artifact
		// through, so every network/evidence claim resolves Unverifiable, never Pass.
		newEnv: func(string) buildEnv { return buildEnv{Box: nil, Verifier: verify.Pass{}} },
	}
	res := sc.run(context.Background(), swarm.Shard{
		ID: "swarm-test-0", Goal: "audit the landing page", Kind: pre.Kind,
		Pack: pre.VerifyPacks[0], Role: string(pre.Role), State: swarm.ShardQueued,
	})
	if res.Passed {
		t.Fatal("ui shard with a nil box must fail closed (Passed:false)")
	}
}

// TestCollateArtifactMakesRedVisibleToScan is the regression guard for the
// false-convergence blocker: a shard's verdict-bearing artifact must be copied into the
// shared collate root for BOTH green AND red shards, and the shard id must be a valid
// single-component artifact id, so requeue.Scan over the collate root actually sees a red
// claim. Before the fix the verifier path was CWD-relative and only green artifacts were
// collated, so a fully-failed run found an empty worklist and falsely converged green.
func TestCollateArtifactMakesRedVisibleToScan(t *testing.T) {
	wt := t.TempDir()
	collate := t.TempDir()
	const id = "swarm-run-0" // the sharder's '-'-delimited shape: a valid artifact id
	red := &artifact.Artifact{
		SchemaVersion: artifact.SchemaVersion, ID: id, Kind: artifact.KindReport,
		Claims: []artifact.Claim{{ID: "c1", Field: "x",
			Evidence: artifact.Evidence{Status: artifact.StatusFail, Verifier: "v"}}},
	}
	if err := artifact.Write(wt, red); err != nil {
		t.Fatalf("seed red artifact: %v", err)
	}

	// The fix: collate copies the RED artifact into the collate root (best-effort).
	collateArtifact(collate, wt, id)

	wl, err := requeue.Scan(collate, nil)
	if err != nil {
		t.Fatalf("requeue.Scan: %v", err)
	}
	if len(wl.Units) != 1 || wl.Units[0].ArtifactID != id || wl.Units[0].Status != artifact.StatusFail {
		t.Fatalf("collated red artifact must surface one red Unit, got %+v", wl.Units)
	}

	// A '/'-containing id (the original blocker) makes artifact.Read reject the path, so
	// collate is a no-op — proving WHY the sharder must mint '-'-delimited ids.
	collateArtifact(collate, wt, "swarm/run/0")
	if wl2, _ := requeue.Scan(collate, nil); len(wl2.Units) != 1 {
		t.Fatalf("an invalid-id collate must be a no-op, got %d units", len(wl2.Units))
	}
}

// TestSwarmResumeAdoptsInterruptedRun asserts --resume adopts an interrupted run's id
// + state from the durable queue (requeue-only-failed), rather than minting a fresh
// run and re-sharding. A run row + one failed shard are persisted, then buildSwarm with
// --resume must seed the run state from them.
func TestSwarmResumeAdoptsInterruptedRun(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	// Persist a prior interrupted run: a run row (SwarmState) + one failed shard with
	// retry budget remaining (so Failed returns it).
	priorRunID := "run-prior"
	q := swarm.NewQueue(st, discardLog(t), priorRunID)
	prior := swarm.SwarmState{RunID: priorRunID, Goal: "prior goal", Preset: "benchmark", Pass: 2,
		Ledger: requeueLedger(3)}
	if err := q.SaveState(ctx, prior); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	failedShard := swarm.Shard{ID: "swarm-" + priorRunID + "-0", Goal: "fix it",
		Kind: artifact.KindBenchmark, Pack: "benchmark", Role: "implementer", State: swarm.ShardFailed}
	if err := q.Mark(ctx, failedShard); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	// buildSwarm with --resume must adopt the prior run's id + state.
	shardFile := filepath.Join(t.TempDir(), "shards.txt")
	_ = writeFile(shardFile, "x\n")
	sf := parseSwarmFlags(t, "-preset", "benchmark", "-shard-file", shardFile, "-resume")
	asm, err := buildSwarm(swarmDeps{flags: sf, boot: testBoot(), log: discardLog(t), dir: newGoRepo(t),
		ledger: budget.New(), pool: testPool(t), store: st})
	if err != nil {
		t.Fatalf("buildSwarm resume: %v", err)
	}
	if asm.runID != priorRunID {
		t.Errorf("resumed runID = %q, want %q (adopt the interrupted run)", asm.runID, priorRunID)
	}
	if asm.state.Pass != 2 {
		t.Errorf("resumed state.Pass = %d, want 2 (threaded forward)", asm.state.Pass)
	}
	if len(asm.initial) != 1 || asm.initial[0].ID != failedShard.ID {
		t.Errorf("resumed initial = %+v, want exactly the one failed shard", asm.initial)
	}
}

// TestSwarmCodeBackendRoutesDelegated asserts pool.CodeBackendFor reports the delegated
// backend the shardFn branches on — so --code-backend codex routes through buildBackend
// (the in-box delegated path), not roster.NewWorker.
func TestSwarmCodeBackendRoutesDelegated(t *testing.T) {
	led := budget.New()
	pl, err := pool.Build(pool.PoolConfig{Worker: pool.TierSpec{CodeBackend: "codex"}}, led, fakeCred, "r", pool.Options{})
	if err != nil {
		t.Fatalf("pool.Build: %v", err)
	}
	if got := pl.CodeBackendFor("implementer"); got != "codex" {
		t.Fatalf("CodeBackendFor = %q, want codex (the delegated route)", got)
	}
	// A native (or unset) worker tier routes through roster.NewWorker.
	natPl, _ := pool.Build(pool.PoolConfig{}, led, fakeCred, "r", pool.Options{})
	if got := natPl.CodeBackendFor("implementer"); got != "native" {
		t.Errorf("CodeBackendFor(unset) = %q, want native", got)
	}
}

// TestSwarmDefaultOffByteIdentical asserts the swarm orchestration packages
// (internal/swarm*) are wired ONLY by the cmd entrypoint, so merely building the
// binary cannot change any other package's behavior. internal/pool is a pool of
// PROVIDERS whose config type (pool.PoolConfig) is the one sanctioned dependency
// onboard holds (SW-T08, a committed config-schema task) — so pool is allowed to be
// imported by onboard (the config type) and cmd, but never reaches the orchestration
// layer either. The check is: no package OTHER than cmd/nilcore, the swarm tree
// itself, or the sanctioned onboard config-holder may import these.
func TestSwarmDefaultOffByteIdentical(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f",
		"{{.ImportPath}} {{join .Imports \" \"}}", "nilcore/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list failed: %v\n%s", err, out)
	}
	// sanctioned importers of internal/pool: cmd (the wiring) + onboard (the committed
	// pool.PoolConfig config-schema field, SW-T08). The swarm tree is checked separately
	// (strictly cmd-only) because it is the orchestration layer.
	poolOK := func(importer string) bool {
		return importer == "nilcore/cmd/nilcore" ||
			importer == "nilcore/internal/onboard" ||
			strings.HasPrefix(importer, "nilcore/internal/pool") ||
			strings.HasPrefix(importer, "nilcore/internal/swarm") // swarm composes the pool
	}
	swarmOK := func(importer string) bool {
		return importer == "nilcore/cmd/nilcore" ||
			strings.HasPrefix(importer, "nilcore/internal/swarm")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		importer := fields[0]
		for _, imp := range fields[1:] {
			if (imp == "nilcore/internal/swarm" || strings.HasPrefix(imp, "nilcore/internal/swarm/")) && !swarmOK(importer) {
				t.Errorf("package %s imports %s — only cmd/nilcore may wire the swarm (default-off regression)", importer, imp)
			}
			if (imp == "nilcore/internal/pool" || strings.HasPrefix(imp, "nilcore/internal/pool/")) && !poolOK(importer) {
				t.Errorf("package %s imports %s — only cmd/nilcore, onboard (config), and swarm may use the pool", importer, imp)
			}
		}
	}
}

// TestSwarmNewLeavesHaveNoInit asserts the new swarm/pool leaves contain no init()
// function — a global-side-effect init could change behavior merely by linking, which
// would break the default-off, byte-identical guarantee.
func TestSwarmNewLeavesHaveNoInit(t *testing.T) {
	pkgs := []string{
		"nilcore/internal/swarm",
		"nilcore/internal/swarm/board",
		"nilcore/internal/swarm/preset",
		"nilcore/internal/pool",
	}
	for _, p := range pkgs {
		// `go doc <pkg> init` is empty when there is no exported/unexported init; instead
		// grep the package sources for a top-level `func init(`.
		out, err := exec.Command("go", "list", "-f", "{{.Dir}}", p).CombinedOutput()
		if err != nil {
			t.Fatalf("go list %s: %v\n%s", p, err, out)
		}
		dir := strings.TrimSpace(string(out))
		gofiles, err := exec.Command("sh", "-c",
			"grep -lE '^func init\\(\\)' "+filepath.Join(dir, "*.go")+" 2>/dev/null || true").CombinedOutput()
		if err != nil {
			t.Fatalf("grep init in %s: %v", dir, err)
		}
		// Exclude *_test.go matches (a test init is allowed and never linked into the binary).
		for _, f := range strings.Fields(string(gofiles)) {
			if !strings.HasSuffix(f, "_test.go") {
				t.Errorf("%s has a global-side-effect init() in %s (default-off regression)", p, f)
			}
		}
	}
}

// --- test helpers -----------------------------------------------------------

// testPool builds a real pool over the fake cred resolver (provider objects are
// constructed, never called) for tests that need a pool but do not exercise a model.
func testPool(t *testing.T) *pool.Pool {
	t.Helper()
	pl, err := pool.Build(pool.PoolConfig{}, budget.New(), fakeCred, "test", pool.Options{})
	if err != nil {
		t.Fatalf("pool.Build: %v", err)
	}
	return pl
}

// testStore opens a fresh temp-file SQLite store for one test (a file, not :memory:,
// matching the queue's own test discipline).
func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "swarm.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// discardLog opens an event log under a temp path (the wiring needs a real *eventlog.Log
// but the test never inspects it).
func discardLog(t *testing.T) *eventlog.Log {
	t.Helper()
	log, err := eventlog.Open(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log
}

// newTestBoard builds a board the shardFn can Record into (a nil ledger reports zero
// cost; the pricer is the conservative table).
func newTestBoard() *board.Board { return board.New(budget.New(), meter.NewTable(), 0) }

// mustBuildPack composes the named pack's verifier with a nil box (the schema layer
// still forms; the evidence layer fails network claims closed). It is the same
// composite the shardFn builds per shard.
func mustBuildPack(t *testing.T, name string) packs.PackPlan {
	t.Helper()
	plan, err := packs.Build(name, nil, ".nilcore/artifacts/x.json", packs.DefaultSchemas())
	if err != nil {
		t.Fatalf("packs.Build %q: %v", name, err)
	}
	return plan
}

// errorsContains reports whether err's message contains sub (a small helper so the
// fail-closed tests assert the message names the offending input).
func errorsContains(err error, sub string) bool {
	return err != nil && strings.Contains(err.Error(), sub)
}

// writeFile writes content to path (a tiny os.WriteFile wrapper kept local so the
// test imports stay minimal).
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// requeueLedger builds a retry ledger with the given per-Unit attempt budget (so a
// freshly-failed shard with no recorded units is eligible for a first retry).
func requeueLedger(maxAttempts int) requeue.Ledger {
	return requeue.Ledger{MaxAttempts: maxAttempts}
}
