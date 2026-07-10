// Package code is the build/test verifier pack (Phase 12, verified swarm mode). It
// is the one pack where the artifact-verifier and the project's own build gate
// overlap: a `spec` artifact's typed claims (asserted by the software pack) are AND'd
// — in the SW-T05 assembler — with the RAW build/test running green in the box. This
// leaf supplies that raw side as two namespaced verifier-ids:
//
//   - code.build_passes — runs the project's build/verify command in the box and
//     asserts a clean (exit-0) build. The command is chosen by REUSING verify.Detect
//     (the conservative make-verify → go → npm → cargo → pytest ladder), so this pack
//     never re-implements detection. It does NOT inherit Detect's "unknown layout ⇒ a
//     no-op `true`" permissiveness, though: for a TYPED claim that asserts "the build
//     passes", a no-op `true` would exit 0 and green the claim with ZERO checking, so an
//     undetectable layout is Unverifiable, never Pass (the same inversion packs/build.go
//     applies to pack NAMES). An operator/claim MAY override the detected command with an
//     allowlisted one (see allowedBuildCommands).
//   - code.test_passes — runs the project's test suite in the box. The model supplies
//     only DATA (a single test selector), single-quoted into a FIXED, pack-authored
//     command shape (`go test`, `npm test`, `pytest`); it can never name a free command.
//     WHERE the selector goes is runner-specific:
//     · go — the selector is a PACKAGE PATH, emitted as a BARE positional arg
//     (`go test '<pkg>'`). It must NOT sit after a literal `--`: `go test -- <x>`
//     hands <x> to the CURRENT-DIRECTORY test binary, so the SELECTED package never
//     runs and a red sub-package would forge a green (I2). A Go test-NAME filter is
//     therefore not supported; the leading-dash rejection is the guard that the
//     selector can never be read as a flag.
//     · npm / pytest — the selector is a test-name/file pattern forwarded to the runner
//     AFTER a literal `--` (`npm test -- '<sel>'`, `pytest -- '<sel>'`), so it can
//     never be read as a runner flag.
//
// Trust disciplines (the same as every shipped pack):
//
//   - I2 (verifier is sole authority): the verdict is the box's exit code over the
//     pack's own command — never the worker's self-claimed Status. Exit 0 ⇒ Pass, a
//     non-zero build/test exit ⇒ Fail (a decisive "it does not build/pass"), a
//     sandbox-level error (the box could not run at all) ⇒ Unverifiable (no decisive
//     verdict). It is fail-closed: an unrecognized/unallowlisted command shape, an
//     UNDETECTABLE build layout (Detect's no-op "true"), or a go run that tested NO
//     package ("matched no packages") ⇒ Unverifiable — never Pass, and never a false Fail.
//   - I4 (sandboxed execution): every command runs via box.Exec INSIDE the worker's
//     sandbox, confined to its worktree. A nil box ⇒ Unverifiable (we refuse a
//     host-side build, which would escape the sandbox boundary). verify.Detect itself
//     only STATS marker files on the host worktree (go.mod, package.json, …) to pick a
//     command string; it executes nothing.
//   - I6 (zero-dep core): stdlib only (plus the in-tree verify/sandbox/artifact/
//     evverify leaves). No build-system module — go.mod is untouched.
//   - I7 (untrusted-as-data): a model-authored field (Value/ExtractionMethod) is
//     validated and single-quoted before it enters a command, and only a bounded,
//     harness-authored detail tail leaves the pack — the raw build output is never
//     echoed unfenced into an Evidence/event field.
//
// This package is a LEAF: it imports only artifact, evverify, sandbox, verify, and the
// standard library — never internal/artifact/schema and never the orchestrator
// (super/roster/agent).
package code

import (
	"context"
	"fmt"
	"strings"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/sandbox"
	"nilcore/internal/verify"
)

// Verifier-ids registered by this pack, namespaced under "code." A claim names e.g.
// Evidence.Verifier = "code.build_passes". Kept in one place so RegisterAll and the
// tests agree on the exact strings.
const (
	IDBuildPasses = "code.build_passes"
	IDTestPasses  = "code.test_passes"
)

// RegisterAll adds this pack's two verifier-ids to r. It registers exactly the build
// and test checks and nothing else — an unregistered id elsewhere stays Unverifiable.
// Registration is the single seam Pillar 2 fills (evverify.Registry); without it
// (packs off) these ids resolve Unverifiable, never Pass.
func RegisterAll(r *evverify.Registry) {
	r.Register(IDBuildPasses, checkBuildPasses)
	r.Register(IDTestPasses, checkTestPasses)
}

// Hosts is the documented egress host-set this pack reaches. The build/test pack is
// purely local — it runs the project's own build and test commands against the
// worktree in the box and touches no remote host — so its catalog is intentionally
// empty (nil). Exposed (like ui.Hosts()) so the packs aggregator's HostsFor("code")
// has a definite, if empty, answer for the egress cross-check.
func Hosts() []string { return nil }

// maxDetail bounds the harness-authored detail tail so a verifier note can never flood
// the artifact JSON or an event Detail. (Kept local so the leaf imports no orchestrator
// package; mirrors evverify's own bound.)
const maxDetail = 512

// detail trims a harness-authored note to the bounded tail. It carries verifier
// commentary ONLY — never the raw build/test output and never a model-authored field
// echoed unfenced (I7).
func detail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxDetail {
		return s[len(s)-maxDetail:]
	}
	return s
}

// allowedBuildCommands is the closed allowlist of build/verify command shapes a claim
// may explicitly request via Evidence.ExtractionMethod (overriding verify.Detect). It
// is deliberately the SAME conservative set verify.Detect itself can emit — so an
// override can only ever NARROW to a recognized command, never introduce a free one.
// A claim that names anything outside this set fails closed to Unverifiable.
var allowedBuildCommands = map[string]bool{
	"make verify":                     true,
	"go build ./... && go test ./...": true,
	"go build ./...":                  true,
	"go test ./...":                   true,
	"npm test":                        true,
	"npm run build":                   true,
	"cargo test":                      true,
	"cargo build":                     true,
	"pytest":                          true,
}

// testRunner is a pack-allowlisted ecosystem's FIXED command shapes. selected is the full
// command TEMPLATE whose single %s is the slot the model-authored selector (DATA) is
// single-quoted into; the template ALSO encodes the runner-specific separator (a bare
// positional arg for go, a literal `--` for npm/pytest — see testRunners). wholeSuite is
// the command run when the selector is EMPTY — it MUST exercise the entire module, never a
// subset. The two are separate because "drop the selector" does not yield a correct
// whole-suite command for every runner: bare `go test` (no package arg) tests ONLY the
// current directory's package, so in a worktree whose tests live in subpackages it
// compiles/runs nothing, prints "[no test files]", and exits 0 — a green with the suite
// never running (the same I2-laundering shape validateSelector blocks for "-run=^$"). The
// go whole-suite form must therefore recurse with "./...". If a runner has no safe
// whole-suite form, wholeSuite is left empty and an empty selector fails closed to
// Unverifiable rather than laundering a green.
type testRunner struct {
	selected   string // full template; its single %s is the single-quoted selector slot
	wholeSuite string // run when the selector is empty; "" ⇒ empty selector is Unverifiable
}

// testRunners is keyed by the token a claim supplies in Evidence.ExtractionMethod. The
// model supplies only the selector (DATA); it can never pick the verb.
var testRunners = map[string]testRunner{
	// go: the selector is a PACKAGE PATH, emitted as a BARE positional arg — NEVER after a
	// literal `--`. `go test -- <x>` hands <x> to the current-directory test binary, so the
	// SELECTED package never runs and a red sub-package would forge a green (I2). The
	// leading-dash rejection in validateSelector is the sole guard that it cannot be a flag.
	// bare `go test` is current-dir only, so the whole-suite form must recurse with "./...".
	"go": {selected: "go test '%s'", wholeSuite: "go test ./..."},
	// npm: the selector is forwarded to the "test" script AFTER `--`; the bare script is the
	// project-defined whole suite already, so the empty-selector form needs no arg.
	"npm": {selected: "npm test -- '%s'", wholeSuite: "npm test"},
	// pytest: the selector is a file/name pattern after `--`; no arg ⇒ recursive discovery
	// from the worktree root — the whole suite.
	"pytest": {selected: "pytest -- '%s'", wholeSuite: "pytest"},
}

// runIn runs cmd in the box and maps the outcome to a Status. It centralizes the
// shared verdict discipline: a nil box ⇒ Unverifiable (refuse a host-side build), a
// sandbox-level error ⇒ Unverifiable (the box could not run the command at all), exit
// 0 ⇒ Pass, any non-zero exit ⇒ Fail (a decisive build/test failure). label names the
// command in the detail tail so a reader can tell build from test without seeing raw
// output.
func runIn(ctx context.Context, box sandbox.Sandbox, label, cmd string) (artifact.Status, string) {
	if box == nil {
		// No sandbox to run in. Refuse rather than fall back to a host-side build,
		// which would escape the sandbox boundary (I4).
		return artifact.StatusUnverifiable, "no sandbox available (refusing host-side build)"
	}
	res, err := box.Exec(ctx, cmd)
	if err != nil {
		// A sandbox-level error (the box could not run the command at all) is not a
		// decisive verdict about the code — fail closed to Unverifiable.
		return artifact.StatusUnverifiable, detail("sandbox: " + err.Error())
	}
	if res.ExitCode == 0 {
		return artifact.StatusPass, fmt.Sprintf("%s exit 0 (%s)", label, cmd)
	}
	// A non-zero exit is NORMALLY a decisive build/test FAILURE. But a go invocation can also
	// exit non-zero when there is simply NOTHING to run — "matched no packages" / "no packages
	// to test" (a worktree with no .go files), or a setup failure with no module. That is not a
	// decisive "the tests failed"; the suite never ran, so it must fail toward Unverifiable —
	// never a false Fail (and never a false Pass). These markers never appear in a genuine
	// `--- FAIL:` test failure, so this can only DOWNGRADE a would-be Fail, never mask a real red.
	if noTestableUnit(res.Stdout + "\n" + res.Stderr) {
		return artifact.StatusUnverifiable, detail(fmt.Sprintf("%s ran no packages (nothing to build/test); not a decisive failure", label))
	}
	// A non-zero exit with something actually built/tested is a decisive failure — Fail, not
	// Unverifiable. We surface only the bounded stderr tail (harness-trimmed), never the full body.
	d := strings.TrimSpace(res.Stderr)
	if d == "" {
		d = fmt.Sprintf("%s exited %d", label, res.ExitCode)
	}
	return artifact.StatusFail, detail(fmt.Sprintf("%s failed (exit %d): %s", label, res.ExitCode, d))
}

// goNoUnitMarkers are the go-tool messages that mean "there was nothing to build or test"
// (an empty/undetectable module), as opposed to a real test failure. They DOWNGRADE a
// non-zero exit from a false Fail to Unverifiable (I2: fail toward no-verdict, never toward
// a spurious pass OR a spurious decisive fail). None of these ever appears in a genuine
// `--- FAIL:` test failure, so the downgrade can never mask a real red.
var goNoUnitMarkers = []string{
	"matched no packages",
	"no packages to test",
	"no Go files in",
	"go.mod file not found",
	"cannot find main module",
	"does not contain main module",
}

// noTestableUnit reports whether combined build/test output indicates the command ran no
// package at all (nothing to test), so its non-zero exit is not a decisive failure.
func noTestableUnit(combined string) bool {
	for _, m := range goNoUnitMarkers {
		if strings.Contains(combined, m) {
			return true
		}
	}
	return false
}

// checkBuildPasses asserts the project builds cleanly in the box. It REUSES
// verify.Detect (over box.Workdir()) to pick the build/verify command — the same
// conservative ladder the native loop uses — or honors a claim-supplied allowlisted
// override. The chosen command runs via box.Exec; exit 0 ⇒ Pass, non-zero ⇒ Fail,
// sandbox error ⇒ Unverifiable. A nil box, an override outside the allowlist, or an
// UNDETECTABLE build layout (verify.Detect's no-op "true") all fail closed to
// Unverifiable — an undetectable build must never green a typed claim on a vacuous no-op.
func checkBuildPasses(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	if box == nil {
		return artifact.StatusUnverifiable, "no sandbox available (refusing host-side build)"
	}

	cmd, err := buildCommand(box, c)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	return runIn(ctx, box, "build", cmd)
}

// noopDetect is the sentinel verify.Detect returns for an unrecognized layout — a command
// that always exits 0. For a typed build claim it must NOT be run (it would green with no
// checking, I2); buildCommand maps it to an Unverifiable error instead.
const noopDetect = "true"

// buildCommand resolves the build command for a claim. An override is read from the
// model-authored Evidence.ExtractionMethod and accepted ONLY if it is in
// allowedBuildCommands (a closed set identical to what verify.Detect can emit); an
// override outside that set is rejected (it must never become a free shell command,
// I7). With no override it REUSES verify.Detect over the box's worktree — the single
// source of truth for "which build command", which this pack must not re-implement — but
// REJECTS Detect's no-op "true" fallback: an undetectable layout yields an error (mapped
// to Unverifiable), never a vacuous always-green command (I2).
func buildCommand(box sandbox.Sandbox, c artifact.Claim) (string, error) {
	override := strings.TrimSpace(c.Evidence.ExtractionMethod)
	if override != "" {
		if !allowedBuildCommands[override] {
			return "", fmt.Errorf("build command %q is not allowlisted", override)
		}
		return override, nil
	}
	cmd := verify.Detect(box.Workdir())
	if cmd == noopDetect {
		// verify.Detect returns the no-op "true" when it recognizes NO build system. For a
		// TYPED claim asserting "the build passes", running a command that always exits 0
		// would green the claim with ZERO checking (I2). Invert Detect's permissiveness here
		// (as packs/build.go does for pack NAMES): an undetectable build is Unverifiable —
		// never a Pass on a vacuous no-op.
		return "", fmt.Errorf("no build system detected for this worktree; refusing the no-op %q (cannot assert a clean build)", noopDetect)
	}
	return cmd, nil
}

// checkTestPasses asserts the project's tests pass in the box. The model supplies only
// DATA: a pack-allowlisted ecosystem token (Evidence.ExtractionMethod ∈ {go,npm,pytest})
// selecting a FIXED command shape, and an optional test SELECTOR (Evidence.Value) that
// is single-quoted into that shape. It can never name a free command. WHERE the selector
// goes is runner-dependent: for `go` it is a PACKAGE PATH emitted as a BARE positional arg
// (`go test '<pkg>'` — NOT after `--`, which would hand it to the current-dir test binary
// and skip the selected package, forging a green — I2; a Go test-NAME filter is not
// supported); for `npm`/`pytest` it is a test-name/file pattern forwarded after a literal
// `--`. exit 0 ⇒ Pass, a decisive non-zero ⇒ Fail, sandbox error / unallowlisted runner /
// unsafe selector / a go run with no packages ⇒ Unverifiable.
func checkTestPasses(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	if box == nil {
		return artifact.StatusUnverifiable, "no sandbox available (refusing host-side test)"
	}

	cmd, err := buildTestCommand(c)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	return runIn(ctx, box, "test", cmd)
}

// buildTestCommand assembles the fixed test command from a claim. The runner token
// (Evidence.ExtractionMethod) MUST be one of testRunners — an empty or unknown token
// fails closed, so a claim can never inject a verb. The optional selector
// (Evidence.Value) is validated (no quote / whitespace / control byte) and
// single-quoted into the shape, so a model-authored selector stays DATA and cannot
// break out to a second command (I7). An EMPTY selector runs the runner's whole-suite
// command (which MUST exercise the entire module — see testRunner); a runner with no safe
// whole-suite form fails closed to Unverifiable rather than laundering a green (I2).
func buildTestCommand(c artifact.Claim) (string, error) {
	token := strings.TrimSpace(c.Evidence.ExtractionMethod)
	if token == "" {
		return "", fmt.Errorf("evidence.extraction_method must name a test runner (one of %s)", knownRunners())
	}
	r, ok := testRunners[token]
	if !ok {
		return "", fmt.Errorf("test runner %q is not allowlisted (one of %s)", token, knownRunners())
	}

	selector := strings.TrimSpace(c.Evidence.Value)
	if selector == "" {
		// No selector — run the WHOLE suite. Use the runner's explicit whole-suite
		// command, NOT the "%s"-stripped shape: for go that would be a bare `go test`
		// (current-dir only), which greens with the suite never running when the tests
		// live in subpackages — the I2-laundering vector. A runner without a safe
		// whole-suite form (empty wholeSuite) fails closed to Unverifiable.
		if r.wholeSuite == "" {
			return "", fmt.Errorf("test runner %q has no whole-suite form; a specific selector is required", token)
		}
		return r.wholeSuite, nil
	}
	if err := validateSelector(selector); err != nil {
		return "", err
	}
	// Single-quote the validated selector into the runner's fixed template. The template
	// already encodes the runner-specific separator (a bare positional arg for go, a literal
	// `--` for npm/pytest — see testRunners), so a leading-dash flag is blocked by
	// validateSelector for every runner and, for npm/pytest, additionally cannot be read as a
	// flag because it sits after `--`. The selector is the Sprintf ARGUMENT (never the format),
	// so a '%' inside it is inert.
	return fmt.Sprintf(r.selected, selector), nil
}

// validateSelector constrains a model-authored test selector before it is placed into
// a single-quoted command argument. It rejects:
//   - a leading '-' — so a selector like "-run=^$" / "-count=0" / "--list" can never be
//     read as a flag that silently selects zero tests (or otherwise neuters the suite)
//     and launders a green verdict past the verifier (I2);
//   - a single quote — which would close the quoting and break the selector out as DATA;
//   - any whitespace or control byte — which could smuggle a flag or a second token.
//
// For npm/pytest, buildTestCommand additionally places the quoted selector after a literal
// "--", so even a future bypass of the leading-dash rule cannot be read as a flag there. The
// go runner cannot use "--" (it would hand the selector to the current-dir test binary
// instead of selecting the package — I2), so for go the leading-dash rejection is the sole
// flag guard. A rejected selector makes the caller fail closed to Unverifiable — never a
// silent broad (or empty) test run.
func validateSelector(sel string) error {
	if strings.HasPrefix(strings.TrimSpace(sel), "-") {
		return fmt.Errorf("test selector may not begin with '-' (it could be read as a flag)")
	}
	for _, r := range sel {
		if r == '\'' {
			return fmt.Errorf("test selector may not contain a single quote")
		}
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("test selector may not contain whitespace or control characters")
		}
	}
	return nil
}

// knownRunners is the sorted token list, used for a helpful error on an unknown runner.
func knownRunners() string {
	out := make([]string, 0, len(testRunners))
	for k := range testRunners {
		out = append(out, k)
	}
	// Small fixed set — a stable, readable order without importing sort is fine, but
	// sort keeps the error deterministic across map-iteration order.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return strings.Join(out, ", ")
}

// ensure the package compiles against the stable CheckFunc signature.
var (
	_ evverify.CheckFunc = checkBuildPasses
	_ evverify.CheckFunc = checkTestPasses
)
