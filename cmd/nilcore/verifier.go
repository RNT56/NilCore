package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"nilcore/internal/artifact/evverify"
	"nilcore/internal/artifact/packs"
	"nilcore/internal/artifact/packs/finance"
	"nilcore/internal/artifact/schema"
	"nilcore/internal/eventlog"
	"nilcore/internal/sandbox"
	"nilcore/internal/secrets"
	"nilcore/internal/verify"
	"nilcore/internal/verify/vcache"
)

// verifyFlagEnabled mirrors the NILCORE_KERNEL default-on idiom (kernel.go,
// kernelEnabled): the feature is the norm, the env var is an instant escape hatch.
// Unset/anything ⇒ on; 0/off/false/no ⇒ off, byte-identical to the undecorated path.
// It is the gate for vcache and flakeprobe, both of which are I2-safe by
// construction (only ever replay/re-run the REAL verifier) and so default ON.
func verifyFlagEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "0", "off", "false", "no":
		return false
	default:
		return true
	}
}

// verifyFlagOptIn is the DEFAULT-OFF converse, the honest posture for a feature that
// is not generically sound. Only 1/on/true/yes turns it on; unset/anything else ⇒
// off. It gates NILCORE_TIERED_VERIFY: a scoped `go vet`/`go test` red is a provable
// subset of the full verify ONLY under narrow conditions (a full-module `go test
// ./...` command, replicated flags, a genuine test/compile red — not a package-load
// error). Since we cannot prove that for an arbitrary repo, the tiered fast path is
// opt-in, not default-on — a false red must never ship as the verdict by default.
func verifyFlagOptIn(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "on", "true", "yes":
		return true
	default:
		return false
	}
}

// vcacheDecorate composes the verify decorator chain around base (the name is kept
// stable for its build.go call site; it grew from the original vcache-only wrap).
// verifierID is the RESOLVED verify command (build.go passes verifyCmd), which both
// keys the cache and drives the tiered soundness gate. vcache and flakeprobe are
// DEFAULT-ON (kernel precedent: each is I2-safe by construction — it only ever
// replays or re-runs the REAL verifier); tiered is DEFAULT-OFF (opt-in), the honest
// posture since a generically-sound scoped-red subset check is not feasible:
//
//	NILCORE_VCACHE=0         — disable the chain-verified pass-replay cache (default on)
//	NILCORE_FLAKEPROBE=0     — disable the one-shot flaky-test re-run     (default on)
//	NILCORE_TIERED_VERIFY=1  — ENABLE the scoped fast red path            (default OFF)
//
// DECORATOR ORDER (outermost → innermost): tiered → flakeprobe → vcache → base.
//
//   - tiered OUTERMOST: the scoped fast check is the cheapest possible signal and
//     most iterations are red — when it fires, nothing inside (no tree hash, no
//     cache lookup, no full verify) runs at all. Its green/error path falls
//     through, so the inner chain is untouched on every conclusive-pass path.
//   - flakeprobe AROUND vcache: the probe's re-run goes back through the cache,
//     which is correct — a failure is never cached, so the probe re-run always
//     reaches the real verifier; and a cache-replayed pass needs no probing.
//     (Inside-out would let a probe bypass the cache for no benefit.)
//   - vcache INNERMOST, hugging base: the identical-content pass replay must key
//     on exactly what the real verifier would run, with no decorator between it
//     and the verdict it records/replays.
//
// I2 holds at every layer: vcache only replays a chain-verified pass the verifier
// itself produced (fail-closed on any chain error); flakeprobe's probe IS the real
// verifier; tiered can only short-circuit RED (its gate below) — the full verifier
// remains the sole source of a PASS. Stages that lack their inputs (nil log, no
// box/workdir, non-Go verify command) are skipped individually; with every flag
// off, base is returned UNCHANGED — byte-identical.
func vcacheDecorate(base verify.Verifier, box sandbox.Sandbox, verifierID string, log *eventlog.Log, logPath string) verify.Verifier {
	v := base
	hasBox := box != nil && box.Workdir() != ""

	// Stage 1 (innermost): the A9 content-hash verify cache (Phase 16, LRN-T05). A
	// chain-verified PASS over the EXACT same worktree content + verifier-id +
	// toolchain is REPLAYED instead of re-run — every successful run otherwise pays
	// a pure-waste second full verify on the unchanged integration tip. I2-safe:
	// vcache.Lookup re-runs eventlog.Verify and FAILS CLOSED to recompute on any
	// chain error; only a pass the inner verifier itself produced is ever replayed.
	if verifyFlagEnabled("NILCORE_VCACHE") && log != nil && logPath != "" && hasBox {
		v = vcache.Decorate(vcache.Config{
			Inner:   v,
			Log:     log,
			LogPath: logPath,
			Hash: func(ctx context.Context) (string, error) {
				// Hash everything the verifier reads (the worktree), skipping VCS/agent state.
				return verify.ContentHashWorktree(ctx, box.Workdir(), ".git", ".nilcore")
			},
			VerifierID: verifierID,
			Toolchain:  verify.Toolchain(),
		})
	}

	// Stage 2: the flake probe — one bounded re-run of the REAL verifier when a
	// test-class failure lands on content identical to the immediately preceding
	// Check (nothing changed, so the red is plausibly nondeterministic). A confirmed
	// flake is recorded as an additive `verify_flaky` event (I5); the probe never
	// invents a verdict — both runs are the authoritative verifier (I2).
	if verifyFlagEnabled("NILCORE_FLAKEPROBE") && hasBox {
		fp := &verify.FlakeProbe{
			Inner: v,
			Hash: func(ctx context.Context) (string, error) {
				return verify.ContentHashWorktree(ctx, box.Workdir(), ".git", ".nilcore")
			},
		}
		if log != nil {
			fp.OnFlaky = func(failClass, contentHash string) {
				log.Append(eventlog.Event{Kind: "verify_flaky", Detail: map[string]any{
					"fail_class":   failClass,
					"content_hash": contentHash,
					"verifier_id":  verifierID,
				}})
			}
		}
		v = fp
	}

	// Stage 3 (outermost): the tiered scoped-red fast path — DEFAULT-OFF (opt-in),
	// double-gated on the opt-in flag AND SOUNDNESS (tieredSound). A scoped
	// `go vet`/`go test` red is a PROVABLE project red only when the resolved verify
	// command is itself a full-module `go test ./...` run whose flags we replicate;
	// an opaque "make verify" recipe is NOT wrapped (it may run no tests / different
	// flags). Only the full verifier can PASS.
	if verifyFlagOptIn("NILCORE_TIERED_VERIFY") && hasBox && tieredSound(verifierID) {
		v = &verify.TieredVerifier{Full: v, ScopedRed: scopedRedFunc(box, verifierID)}
	}
	return v
}

// tieredSound is the I2-soundness gate for the tiered wrap: a scoped go-test red may
// only short-circuit the full verify when it is PROVABLY a subset of what the full
// verify would find. That requires the resolved verify command to be a full-module
// `go test ./...` invocation:
//
//   - it must contain `go test` as a command (word-boundary match), AND
//   - it must run over the whole module (`./...`), so every package the scoped
//     `go test <touched-pkgs>` compiles/tests is one the full command compiles/tests
//     too — making the scoped red a strict subset (verify.Detect's go.mod fallback
//     "go build ./... && go test ./..." is the canonical hit).
//
// An opaque "make verify" is deliberately NOT armed. Its recipe is unknown from this
// layer: it might run no tests, `go test -short`, or a bespoke script, so a scoped
// `go vet`/`go test` red could red on something the recipe never gates — a FALSE red
// shipped as the verdict. Correctness beats latency; when the command is not a
// transparent full-module go-test run we fall through to the full verify.
//
// Any other command (npm test, cargo test, pytest, "true", `go test ./pkg` on a
// single package, a custom script) leaves the verifier UNWRAPPED — we cannot prove a
// scoped Go red is that project's red.
func tieredSound(verifyCmd string) bool {
	c := strings.TrimSpace(verifyCmd)
	return containsGoTest(c) && strings.Contains(c, "./...")
}

// containsGoTest reports whether cmd invokes `go test` as a COMMAND — the match
// must sit on a word boundary, because a naive substring check is unsound:
// "cargo test" contains the bytes "go test" ("car|go test") but runs no Go test.
func containsGoTest(cmd string) bool {
	for i := 0; ; {
		j := strings.Index(cmd[i:], "go test")
		if j < 0 {
			return false
		}
		j += i
		if j == 0 || !isWordByte(cmd[j-1]) {
			return true
		}
		i = j + 1
	}
}

// isWordByte reports whether b could be part of a longer program name, which
// would make a following "go test" a false command match (e.g. the 'r' in
// "cargo test"). Separators like space, ';', '&', '(' or a path '/' are fine.
func isWordByte(b byte) bool {
	return b == '_' || b == '-' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// scopedRedFunc builds the TieredVerifier.ScopedRed seam: discover the packages
// touched since the run baseline via git, then run a targeted `go test` (with the
// real command's flags replicated) over just those packages — all through the SAME
// sandbox exec path the full verifier uses (I4: nothing here executes on the host).
// Every inconclusive outcome (git fault, unscopable change, no touched Go packages,
// an AMBIGUOUS nonzero exit that is not a genuine test/compile red) returns
// failed=false or an error, which the decorator treats as "fall through to Full" —
// under-scoping and ambiguity can only cost speed, never correctness.
//
// Baseline note: the diff is `git diff --name-only HEAD` (uncommitted work) plus
// untracked files — the simplest baseline reachable from this wiring layer. If the
// worker has already committed (empty diff), the scoped set is empty and we fall
// through to the full verify: a too-small touched set is always sound, because a
// scoped GREEN never decides anything.
//
// FLAG REPLICATION: the scoped `go test` copies the full command's go-test flags
// (-short/-tags/-count/-race) via goTestFlags, so the scoped run tests EXACTLY what
// the full `go test ./...` would over those packages. Dropping -short (or -tags)
// would let the scoped run red on a test/build the full command never executes — a
// false red. `go vet` is folded in only when the resolved command visibly runs it
// (for a plain "go test ./..." project a vet-only red is NOT a project red, so the
// scoped check is `go test` alone — whose red is always a subset red, since go test
// compiles its packages).
//
// PROVABLE-RED GATE: a nonzero scoped exit is treated as a short-circuit red ONLY
// when scopedRedIsProvable confirms it is a genuine TEST FAILURE or COMPILE error in
// a touched package. A package-LOAD/resolution error (a nested go.mod, an unknown
// import, `go: ...`) exits nonzero without any package having failed a test the full
// command would gate — so it falls through to Full rather than shipping as a red.
func scopedRedFunc(box sandbox.Sandbox, verifyCmd string) func(ctx context.Context) (bool, string, error) {
	includeVet := strings.Contains(verifyCmd, "go vet")
	flags := goTestFlags(verifyCmd)
	return func(ctx context.Context) (failed bool, output string, err error) {
		// (a) Touched files since the run baseline, through the sandbox git.
		res, err := box.Exec(ctx, "git diff --name-only HEAD && git ls-files --others --exclude-standard")
		if err != nil {
			return false, "", err
		}
		if res.ExitCode != 0 {
			return false, "", fmt.Errorf("scoped diff: exit %d", res.ExitCode)
		}
		pkgs, ok := touchedGoPackageDirs(box.Workdir(), res.Stdout)
		if !ok || len(pkgs) == 0 {
			return false, "", nil // unscopable or nothing touched ⇒ Full decides
		}

		// (b) The targeted red-detector over exactly the touched packages, with the
		// full command's flags replicated so it runs the same subset the full does.
		list := strings.Join(pkgs, " ")
		test := "go test"
		if flags != "" {
			test += " " + flags
		}
		test += " " + list
		cmd := test
		if includeVet {
			cmd = "go vet " + list + " && " + cmd
		}
		r, err := box.Exec(ctx, cmd)
		if err != nil {
			return false, "", err
		}
		out := strings.TrimSpace(r.Stdout + "\n" + r.Stderr)
		if r.ExitCode == 0 {
			return false, out, nil // scoped green decides nothing ⇒ Full
		}
		// A nonzero exit is a verdict-worthy red ONLY if it is a PROVABLE subset red.
		// An ambiguous nonzero (package-load/resolution error, a vet-only nit the full
		// command would not gate) is inconclusive ⇒ fall through to Full.
		if !scopedRedIsProvable(out) {
			return false, "", nil
		}
		return true, out, nil
	}
}

// goTestFlags extracts the go-test flags from the resolved verify command so the
// scoped `go test` replicates the full run's behavior over the touched packages.
// Only the flags that change WHICH tests/builds run (and can thus flip a red) are
// copied — -short, -tags <v>/-tags=<v>, -count <v>/-count=<v>, -race. A dropped flag
// would make the scoped run diverge from the full command's subset and red falsely
// (e.g. -short skips a long test the full command also skips). It scans the whitespace
// tokens of the command; the verify command is a harness-resolved string (verify.Detect
// / operator-set), not model-authored, so a simple token scan is sufficient here.
func goTestFlags(verifyCmd string) string {
	toks := strings.Fields(verifyCmd)
	var out []string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t == "-short" || t == "-race":
			out = append(out, t)
		case t == "-tags" || t == "-count":
			// space-separated value form: "-tags integration".
			out = append(out, t)
			if i+1 < len(toks) {
				out = append(out, toks[i+1])
				i++
			}
		case strings.HasPrefix(t, "-tags=") || strings.HasPrefix(t, "-count="):
			out = append(out, t) // "=" form carries its own value.
		}
	}
	return strings.Join(out, " ")
}

// scopedRedIsProvable reports whether a nonzero `go test` output is a GENUINE test
// failure or compile error in a touched package — the only red we may ship as the
// verdict (a strict subset of what the full `go test ./...` would find). It returns
// false for a package-LOAD/resolution error, which exits nonzero WITHOUT any package
// having failed a gated check: a nested go.mod, an unresolved import, a missing
// module — the full command would surface these too, but as its OWN red, not this
// scoped run's, so shipping the scoped red here could mislabel a non-subset failure.
// On any ambiguity we return false and let Full decide (correctness > latency).
//
// go test's output is structured enough to classify structurally:
//   - a genuine failure prints "--- FAIL", "FAIL\t<pkg>", or a compile error
//     "<file>.go:NN: ..." under a "# <pkg>" build header ⇒ provable subset red;
//   - a load/resolution error prints a top-level "go: ..." line (module/toolchain)
//     or "no required module provides package" / "cannot find package" with NO test
//     or build failure ⇒ NOT provable ⇒ fall through.
func scopedRedIsProvable(output string) bool {
	// Load/resolution markers: a bare `go:` line or an unresolved-package message is a
	// toolchain/module fault, not a package's own test/compile red.
	loadMarkers := []string{
		"no required module provides package",
		"cannot find package",
		"go.mod file not found",
		"malformed module path",
		"unknown directive",
		"updates to go.mod needed",
		"missing go.sum entry",
	}
	for _, m := range loadMarkers {
		if strings.Contains(output, m) {
			return false
		}
	}
	// A genuine test failure or a compile error in a package.
	if strings.Contains(output, "--- FAIL") ||
		strings.Contains(output, "\nFAIL") || strings.HasPrefix(output, "FAIL") ||
		strings.Contains(output, ".go:") { // "<file>.go:NN:" compile-error location
		return true
	}
	// A line beginning "go:" (module/toolchain diagnostic) with no failure above ⇒ load.
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "go: ") {
			return false
		}
	}
	// Anything else nonzero is ambiguous — fall through to Full.
	return false
}

// touchedGoPackageDirs maps a git name-list (one path per line, relative to the
// worktree root) to the deduped, sorted package-dir patterns to vet/test. It
// returns ok=false when the change set cannot be SOUNDLY scoped, so the caller
// falls through to the full verify:
//
//   - any touched non-.go file (go.mod, Makefile, testdata, generated inputs) can
//     affect arbitrary packages — unscopable;
//   - any path with characters outside a conservative allowlist is refused: these
//     names are model-authored file paths and are being folded into a sandboxed
//     shell command line, so hygiene demands rejecting anything shell-significant
//     (falling through to Full is always sound) rather than quoting cleverly (I7);
//   - a touched dir that no longer exists or holds no .go files (a deleted
//     package) is SKIPPED, not tested: `go test` on a vanished dir would be a
//     false red, while any breakage its deletion causes elsewhere is caught by
//     the full verify that every scoped-green run still falls through to.
//
// Paths under .nilcore/ are ignored (agent scratch state, mirrored from the
// content-hash skip set). The existence probe is a host-side READ of the worktree
// (like artifactFiles above) — discovery only, never execution.
func touchedGoPackageDirs(root, nameList string) ([]string, bool) {
	seen := map[string]bool{}
	var dirs []string
	for _, line := range strings.Split(nameList, "\n") {
		rel := strings.TrimSpace(line)
		if rel == "" || strings.HasPrefix(rel, ".nilcore/") {
			continue
		}
		if !strings.HasSuffix(rel, ".go") {
			return nil, false // a non-Go file can affect the world ⇒ unscopable
		}
		if !safeScopedPath(rel) {
			return nil, false
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if !dirHasGoFiles(root, dir) {
			continue // deleted/emptied package: skip; Full still guards the fallout
		}
		if dir == "." {
			dirs = append(dirs, ".")
		} else {
			dirs = append(dirs, "./"+dir)
		}
	}
	sort.Strings(dirs)
	return dirs, true
}

// safeScopedPath allowlists the characters a touched path may contain before it is
// folded into the scoped command line: letters, digits, '_', '-', '.', '/'. It
// also refuses a leading '-' (flag injection) and any ".." segment. Anything
// outside the allowlist ⇒ unscopable, never quoted-and-hoped.
func safeScopedPath(rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "-") || strings.Contains(rel, "..") {
		return false
	}
	for _, r := range rel {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == '/':
		default:
			return false
		}
	}
	return true
}

// dirHasGoFiles reports whether dir (relative to root) still exists and directly
// contains at least one .go file — i.e. `go test ./dir` has something to build.
func dirHasGoFiles(root, dir string) bool {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

// behavioralVerifier builds the project verifier, optionally composed with a
// headless-browser behavioral check (P9-T03) and/or an evidence-artifact check
// (P11-T05). When NILCORE_BROWSER_VERIFY is set (to the in-sandbox browser-driver
// command that navigates the running app and exits non-zero on a broken render),
// the verdict ANDs the project's own checks with a verify.BrowserVerifier — so a
// change that builds and tests green but renders broken still ships RED. When
// NILCORE_EVIDENCE_VERIFY is set AND the worktree carries one or more artifact
// files (.nilcore/artifacts/<id>.json), the verdict ALSO ANDs in an
// evverify.ArtifactVerifier per artifact, so a report/matrix/dossier whose claims
// did not each pass a runnable check ships RED (I2). The verifier stays the sole
// authority on "done" (I2); a behavioral or evidence result is an INPUT to the
// verdict, never a self-report. Unset ⇒ exactly verify.New (byte-identical).
//
// It is applied to whole-app drives (run / chat / serve / resume), not to
// individual build subagents — a behavioral check belongs at the app level, not
// per-component. (Per-subagent evidence verification is composed into env.Verifier
// separately by P11-T16.) These app-level call sites do not thread an event log, so
// the evidence checks here run with a nil EventSink; the eventlog-backed sink is
// supplied by behavioralVerifierWithLog (and reused by P11-T16's env.Verifier).
func behavioralVerifier(box sandbox.Sandbox, cmd string) verify.Verifier {
	return behavioralVerifierWithLog(box, cmd, nil)
}

// orchestratorVerifier is the verifier for the single-task orchestrator paths
// (run / serve / chat / resume): the behavioral verifier wrapped by vcacheDecorate,
// exactly as buildEnvFactory does for the build/swarm paths. Wiring it here is what
// makes the shipped verify decorators actually reach these paths — the ONE verifier
// instance is shared by the native backend's finish-verify AND the orchestrator's
// post-run re-verify, so vcache REPLAYS the identical-content pass (killing the 2x
// full verify on every green run) and FlakeProbe re-runs the real verifier once when
// the orchestrator's final verify reddens on content a preceding check just passed —
// defusing the N-worktree race a coin-flip test would otherwise trigger (RaceN lives
// only on these paths). I2 is intact: only the full verifier ever produces a PASS,
// and every decorator is nil/flag-gated (see vcacheDecorate).
func orchestratorVerifier(box sandbox.Sandbox, cmd string, log *eventlog.Log, logPath string) verify.Verifier {
	return vcacheDecorate(behavioralVerifierWithLog(box, cmd, log), box, cmd, log, logPath)
}

// behavioralVerifierWithLog is the log-bearing form of behavioralVerifier: when a
// non-nil eventlog is supplied AND evidence verification is enabled, each
// ArtifactVerifier emits its additive artifact_verify/claim_verify events through
// the eventlog (I5 — new append-only kinds, never a mutation). behavioralVerifier
// delegates here with a nil log so the existing app-level call sites stay
// byte-identical and emit no evidence events; a future log-bearing caller (P11-T16)
// passes its run log to get the audit trail. With every evidence/browser toggle off
// this returns exactly verify.New(box, cmd) — the unset path is byte-identical.
func behavioralVerifierWithLog(box sandbox.Sandbox, cmd string, log *eventlog.Log) verify.Verifier {
	base := verify.New(box, cmd)

	var extra []verify.NamedVerifier
	if bcmd := strings.TrimSpace(os.Getenv("NILCORE_BROWSER_VERIFY")); bcmd != "" {
		extra = append(extra, verify.NamedVerifier{Name: "browser", V: verify.NewBrowser(box, bcmd)})
	}
	extra = append(extra, evidenceVerifiers(box, log)...)

	if len(extra) == 0 {
		// No behavioral/evidence checks opted in: return the bare project verifier
		// exactly as before, so the default path is byte-identical (P11-T05/P9-T03).
		return base
	}

	// Named[0] is always the build/"checks" verifier, so an evidence or browser
	// check can never mask a red build: Composite short-circuits on the first
	// failure and the build verifier runs first (I2).
	named := make([]verify.NamedVerifier, 0, 1+len(extra))
	named = append(named, verify.NamedVerifier{Name: "checks", V: base})
	named = append(named, extra...)
	return verify.Composite{Named: named}
}

// evidenceVerifiers returns one trailing NamedVerifier per artifact file present in
// the worktree, gated on NILCORE_EVIDENCE_VERIFY. It is the P11-T05 wiring seam:
//
//   - Env unset                       ⇒ nil (no evidence verifier; byte-identical).
//   - Env set, no artifact file       ⇒ nil (a green build still greens — an
//     evidence verifier is only added when there is
//     something to assert over).
//   - Env set, artifact file(s) found ⇒ one ArtifactVerifier per file, each composed
//     after the build verifier so any red claim
//     reddens the whole verdict (I2).
//
// The registry starts at evverify.Default() — only safe, generic stdlib checks; an
// unregistered verifier-id resolves to StatusUnverifiable, never Pass. When
// NILCORE_VERIFY_PACKS names one or more domain packs (web/software/finance/ui), those
// packs' RegisterAll ids are added on top (P11-T12) so a claim naming e.g.
// finance.sec_fact resolves to a real check instead of Unverifiable-by-missing-id.
// Every check reaches the network only through the box (I4); a nil box fails network
// claims closed to Unverifiable with no host-side request. MaxAge comes from
// NILCORE_EVIDENCE_MAX_AGE (0/unset ⇒ staleness disabled); it can only DEMOTE a pass to
// stale, never be the sole basis to PASS (I2).
//
// Pack selection is fail-closed: an unknown pack name (a typo in NILCORE_VERIFY_PACKS)
// makes every artifact verifier RED via the always-fail sentinel rather than silently
// dropping the requested check — so a misconfigured run never greens by ignoring a pack
// it was told to run. The explicit startup signal lives in validateVerifyPacks.
func evidenceVerifiers(box sandbox.Sandbox, log *eventlog.Log) []verify.NamedVerifier {
	if strings.TrimSpace(os.Getenv("NILCORE_EVIDENCE_VERIFY")) == "" {
		return nil
	}
	if box == nil {
		// No worktree to scan and no box to verify through. There is nothing to assert
		// over; leave the verdict to the build verifier rather than fabricate a check.
		return nil
	}

	paths := artifactFiles(box.Workdir())
	if len(paths) == 0 {
		return nil
	}

	maxAge := evidenceMaxAge()
	sink := evidenceEventSink(log)

	reg, err := evidenceRegistry()
	if err != nil {
		// Fail-closed: a bad pack list (unknown name) must not silently fall back to the
		// generic-only registry — that would green a finance/ui claim as a no-op. Redden
		// the whole evidence verdict with a single named failure carrying the reason.
		return []verify.NamedVerifier{{Name: "evidence:packs", V: failClosed{reason: err.Error()}}}
	}

	// The shape gate (structural, no box, no network) — the SAME catalog the swarm path
	// uses in packs.Build. Composing it here too means the run/chat/serve evidence path
	// enforces the identical acceptance bar (CitationRequired, MinClaims, VerifierRequired,
	// duplicate-id detection, Kind match) instead of only the per-claim ArtifactVerifier —
	// so a structurally degenerate artifact (a one-row matrix, an uncited report claim) is
	// rejected on every ship path, not just under swarm. It runs FIRST per artifact, so a
	// shape defect short-circuits before the (network/box) claim checks run.
	schemaReg := schema.Default()

	out := make([]verify.NamedVerifier, 0, len(paths)*2)
	for _, p := range paths {
		out = append(out, verify.NamedVerifier{
			Name: "schema:" + artifactID(p),
			V:    &schema.SchemaVerifier{Reg: schemaReg, RelPath: p},
		})
		av := &evverify.ArtifactVerifier{
			Box:       box,
			Reg:       reg,
			RelPath:   p,
			MaxAge:    maxAge,
			EventSink: sink,
		}
		out = append(out, verify.NamedVerifier{Name: "evidence:" + artifactID(p), V: av})
	}
	return out
}

// artifactFiles returns the absolute paths of every .nilcore/artifacts/*.json file
// in the worktree, sorted for a stable verifier order. It is a host-side READ of the
// worktree the app verifier owns purely to discover which artifacts exist; the actual
// load is done inside evverify via worktreefs (O_NOFOLLOW), so a symlink swapped in at
// a target path is still refused there. A missing/empty directory yields no paths
// (evidence verification is then a no-op — the green-build path stays green).
func artifactFiles(root string) []string {
	if root == "" {
		return nil
	}
	dir := filepath.Join(root, ".nilcore", "artifacts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	sort.Strings(paths)
	return paths
}

// artifactID recovers the artifact id from its file path for the NamedVerifier label
// (a human-readable failure prefix only — never a trust input).
func artifactID(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".json")
}

// evidenceMaxAge reads the optional staleness window from NILCORE_EVIDENCE_MAX_AGE
// (a Go duration, e.g. "24h"). Unset/blank/invalid ⇒ 0, which disables staleness
// (MaxAge can only DEMOTE a verified pass to stale, never PASS on a timestamp — I2).
func evidenceMaxAge() time.Duration {
	raw := strings.TrimSpace(os.Getenv("NILCORE_EVIDENCE_MAX_AGE"))
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// evidenceEventSink adapts the evverify EventSink callback to the append-only event
// log (I5). It emits two additive kinds, Detail-only and metadata-only:
//
//   - claim_verify    {claim_id, field, verifier, status, source_url}
//   - artifact_verify {id, kind, green, pass, fail, stale, unverifiable}
//
// Both carry ONLY harness-trusted fields plus the claim's key-free SourceURL (I3 —
// provenance is required key-free; the model-authored Value/Statement are never
// echoed, I7). The eventlog redaction path still runs over every Detail, so a secret
// that somehow reached a field is scrubbed. A nil log ⇒ nil sink ⇒ no events emit and
// the verifier behaves byte-identically (the unset/log-less app path).
func evidenceEventSink(log *eventlog.Log) func(ev any) {
	if log == nil {
		return nil
	}
	return func(ev any) {
		switch e := ev.(type) {
		case evverify.ClaimVerifyEvent:
			log.Append(eventlog.Event{Kind: "claim_verify", Detail: map[string]any{
				"claim_id":   e.ClaimID,
				"field":      e.Field,
				"verifier":   e.Verifier,
				"status":     string(e.Status),
				"source_url": e.SourceURL,
			}})
		case evverify.ArtifactVerifyEvent:
			log.Append(eventlog.Event{Kind: "artifact_verify", Detail: map[string]any{
				"id":           e.ArtifactID,
				"kind":         string(e.Kind),
				"green":        e.Green,
				"pass":         e.Pass,
				"fail":         e.Fail,
				"stale":        e.Stale,
				"unverifiable": e.Unverifiable,
			}})
		}
	}
}

// verifyPacks parses the opt-in NILCORE_VERIFY_PACKS / -verify-packs list into the
// pack names to register on top of evverify.Default(). Names are comma-separated and
// (per packs.Select) case-insensitive + space-trimmed; an empty/blank list returns nil,
// the byte-identical default where the registry equals evverify.Default() and any
// pack-claim resolves Unverifiable rather than Pass.
func verifyPacks() []string {
	raw := strings.TrimSpace(os.Getenv("NILCORE_VERIFY_PACKS"))
	if raw == "" {
		return nil
	}
	var names []string
	for _, part := range strings.Split(raw, ",") {
		if n := strings.TrimSpace(part); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// evidenceRegistry builds the verifier registry the evidence verifiers run against:
// evverify.Default() (generic stdlib checks only) plus exactly the packs named in
// NILCORE_VERIFY_PACKS. With no packs opted in it returns Default() unchanged — the
// byte-identical P11-T05 state. packs.Select is ATOMIC: an unknown pack name aborts
// before any registration, so the returned error means NOTHING was registered and the
// caller fails the verdict closed rather than running a half-populated registry.
//
// Before returning, keyed packs' API keys are seeded from the SecretStore into the
// process environment by NAME (env-first, then SecretStore — mirroring the credential
// resolver at main.go). The pack itself references the key by $NAME and injects the
// VALUE via box.ExecWithEnv for a single invocation; the literal key never enters the
// command string, the persisted Evidence.SourceURL, or any event Detail (I3).
func evidenceRegistry() (*evverify.Registry, error) {
	reg := evverify.Default()
	if names := verifyPacks(); len(names) > 0 {
		if err := packs.Select(names, reg); err != nil {
			return nil, err
		}
		seedKeyedPackSecrets(names)
	}
	return reg, nil
}

// validateVerifyPacks is the explicit startup signal that the opted-in pack list is
// resolvable: it returns a non-nil error for an unknown pack name so a misconfigured
// run can fail loudly at boot instead of only reddening at verify time. It is a pure
// validation (a throwaway registry), safe to call before any verification. Empty list
// (packs off) ⇒ nil.
func validateVerifyPacks() error {
	names := verifyPacks()
	if len(names) == 0 {
		return nil
	}
	if err := packs.Select(names, evverify.New()); err != nil {
		return fmt.Errorf("NILCORE_VERIFY_PACKS: %w", err)
	}
	return nil
}

// keyedPackEnv maps each pack name to the SecretStore-resolvable env var NAMES its
// keyed checks reference. Only the NAME lives here (and in the pack leaf); the VALUE is
// resolved from the SecretStore at wiring time and injected per-invocation by the pack
// via box.ExecWithEnv. Keyless packs have no entry.
var keyedPackEnv = map[string][]string{
	packs.NameFinance: {finance.EnvFREDKey, finance.EnvMarketKey},
}

// anyKeyedPack reports whether any selected pack has keyed checks (an entry in
// keyedPackEnv). It gates the SecretStore lookup so a keyless selection never probes the
// host store.
func anyKeyedPack(names []string) bool {
	for _, raw := range names {
		if _, ok := keyedPackEnv[strings.ToLower(strings.TrimSpace(raw))]; ok {
			return true
		}
	}
	return false
}

// secretStoreForPacks is the SecretStore the keyed-pack key resolution reads from. It is
// a package var so tests can inject a hermetic fake; when nil and a keyed pack is opted
// in, seedKeyedPackSecrets falls back to the host store (secrets.Detect) so the default
// boot path resolves keys without a main.go edit. It is consulted ONLY when a keyed pack
// is actually selected, so the packs-off path performs no SecretStore lookup and stays
// byte-identical.
var secretStoreForPacks secrets.SecretStore

// seedKeyedPackSecrets resolves each opted-in keyed pack's API keys env-first, then from
// the SecretStore, and seeds any value found (and not already present) into the process
// environment by NAME. This is the SecretStore → box.ExecWithEnv hop required by I3: the
// pack reads the NAME at run time and routes the VALUE through ExecWithEnv, so the key
// never lands in the command string, the artifact JSON, or an event Detail. A missing
// secret leaves the env untouched (a keyed check with no key supplied then resolves
// Unverifiable, never Pass). The host store is detected lazily and ONLY when a keyed pack
// was selected, so the default packs-off path never probes the keychain.
func seedKeyedPackSecrets(names []string) {
	// Only packs with keyed checks need a store; skip the lookup (and the keychain probe)
	// entirely when none of the selected packs is keyed.
	if !anyKeyedPack(names) {
		return
	}
	store := secretStoreForPacks
	if store == nil {
		store = secrets.Detect()
	}
	if store == nil {
		return
	}
	for _, raw := range names {
		envNames, ok := keyedPackEnv[strings.ToLower(strings.TrimSpace(raw))]
		if !ok {
			continue
		}
		for _, name := range envNames {
			if strings.TrimSpace(os.Getenv(name)) != "" {
				continue // env-first: an operator-set value wins, no SecretStore read
			}
			if v, err := store.Get(name); err == nil && v != "" {
				_ = os.Setenv(name, v)
			}
		}
	}
}

// failClosed is a verify.Verifier that always reports RED with a fixed reason. It is the
// fail-closed sentinel for a wiring error (e.g. an unknown pack name): rather than run a
// silently-degraded registry, the evidence verdict carries one named failure so the run
// reds and the operator sees why.
type failClosed struct{ reason string }

func (f failClosed) Check(context.Context) (verify.Report, error) {
	return verify.Report{Passed: false, Output: f.reason}, nil
}
