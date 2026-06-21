// Package code is the build/test verifier pack (Phase 12, verified swarm mode). It
// is the one pack where the artifact-verifier and the project's own build gate
// overlap: a `spec` artifact's typed claims (asserted by the software pack) are AND'd
// — in the SW-T05 assembler — with the RAW build/test running green in the box. This
// leaf supplies that raw side as two namespaced verifier-ids:
//
//   - code.build_passes — runs the project's build/verify command in the box and
//     asserts a clean (exit-0) build. The command is chosen by REUSING verify.Detect
//     (the conservative make-verify → go → npm → cargo → pytest ladder), so this pack
//     never re-implements detection and inherits its "unknown layout ⇒ a no-op `true`,
//     never a spurious red" discipline. An operator/claim MAY override the detected
//     command with an allowlisted one (see allowedBuild).
//   - code.test_passes — runs the project's test suite in the box. The model supplies
//     only DATA (a single test selector — a package path / test name / file) which is
//     single-quoted into a FIXED, pack-authored command shape (`go test`, `npm test`,
//     `pytest`); it can never name a free command.
//
// Trust disciplines (the same as every shipped pack):
//
//   - I2 (verifier is sole authority): the verdict is the box's exit code over the
//     pack's own command — never the worker's self-claimed Status. Exit 0 ⇒ Pass, a
//     non-zero build/test exit ⇒ Fail (a decisive "it does not build/pass"), a
//     sandbox-level error (the box could not run at all) ⇒ Unverifiable (no decisive
//     verdict). It is fail-closed: an unrecognized/unallowlisted command shape ⇒
//     Unverifiable, never Pass.
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

// runner names the FIXED command shape a test selector is single-quoted into, keyed by
// a pack-allowlisted ecosystem token the claim supplies in Evidence.ExtractionMethod.
// The model supplies only the selector (DATA); it can never pick the verb. An empty
// selector runs the bare suite (the value of the map with %s ⇒ no trailing selector
// handled by buildTestCommand).
var testRunners = map[string]string{
	"go":     "go test %s",
	"npm":    "npm test %s",
	"pytest": "pytest %s",
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
	// A non-zero exit is a decisive failure of the build/test — Fail, not Unverifiable.
	// We surface only the bounded stderr tail (harness-trimmed), never the full body.
	d := strings.TrimSpace(res.Stderr)
	if d == "" {
		d = fmt.Sprintf("%s exited %d", label, res.ExitCode)
	}
	return artifact.StatusFail, detail(fmt.Sprintf("%s failed (exit %d): %s", label, res.ExitCode, d))
}

// checkBuildPasses asserts the project builds cleanly in the box. It REUSES
// verify.Detect (over box.Workdir()) to pick the build/verify command — the same
// conservative ladder the native loop uses — or honors a claim-supplied allowlisted
// override. The chosen command runs via box.Exec; exit 0 ⇒ Pass, non-zero ⇒ Fail,
// sandbox error ⇒ Unverifiable. A nil box, or an override outside the allowlist, fails
// closed to Unverifiable.
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

// buildCommand resolves the build command for a claim. An override is read from the
// model-authored Evidence.ExtractionMethod and accepted ONLY if it is in
// allowedBuildCommands (a closed set identical to what verify.Detect can emit); an
// override outside that set is rejected (it must never become a free shell command,
// I7). With no override it REUSES verify.Detect over the box's worktree — the single
// source of truth for "which build command", which this pack must not re-implement.
func buildCommand(box sandbox.Sandbox, c artifact.Claim) (string, error) {
	override := strings.TrimSpace(c.Evidence.ExtractionMethod)
	if override != "" {
		if !allowedBuildCommands[override] {
			return "", fmt.Errorf("build command %q is not allowlisted", override)
		}
		return override, nil
	}
	return verify.Detect(box.Workdir()), nil
}

// checkTestPasses asserts the project's tests pass in the box. The model supplies only
// DATA: a pack-allowlisted ecosystem token (Evidence.ExtractionMethod ∈ {go,npm,pytest})
// selecting a FIXED command shape, and an optional test SELECTOR (Evidence.Value — a
// package path / test name / file) that is single-quoted into that shape. It can never
// name a free command. exit 0 ⇒ Pass, non-zero ⇒ Fail, sandbox error / unallowlisted
// runner / unsafe selector ⇒ Unverifiable.
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
// break out to a second command (I7). An empty selector runs the bare suite.
func buildTestCommand(c artifact.Claim) (string, error) {
	token := strings.TrimSpace(c.Evidence.ExtractionMethod)
	if token == "" {
		return "", fmt.Errorf("evidence.extraction_method must name a test runner (one of %s)", knownRunners())
	}
	shape, ok := testRunners[token]
	if !ok {
		return "", fmt.Errorf("test runner %q is not allowlisted (one of %s)", token, knownRunners())
	}

	selector := strings.TrimSpace(c.Evidence.Value)
	if selector == "" {
		// No selector — run the whole suite. Trim the "%s" placeholder and its
		// surrounding space so we don't pass a stray empty quoted arg.
		bare := strings.TrimSpace(strings.ReplaceAll(shape, "%s", ""))
		return bare, nil
	}
	if err := validateSelector(selector); err != nil {
		return "", err
	}
	// Insert a literal "--" before the single-quoted selector so the test runner
	// reads it strictly as a positional argument, never as a flag — a defense-in-depth
	// behind validateSelector's leading-dash rejection. Even a future bypass of that
	// guard cannot turn a selector like "-run=^$" into a flag that selects zero tests
	// and launders a green verdict (I2).
	return fmt.Sprintf(strings.ReplaceAll(shape, "%s", "-- '%s'"), selector), nil
}

// validateSelector constrains a model-authored test selector before it is placed into
// a single-quoted command argument. It rejects:
//   - a leading '-' — so a selector like "-run=^$" / "-count=0" / "--list" can never be
//     read as a flag that silently selects zero tests (or otherwise neuters the suite)
//     and launders a green verdict past the verifier (I2);
//   - a single quote — which would close the quoting and break the selector out as DATA;
//   - any whitespace or control byte — which could smuggle a flag or a second token.
//
// buildTestCommand additionally prefixes a literal "--" before the quoted selector, so
// even a future bypass of the leading-dash rule cannot be read as a flag. Together these
// mirror the URL/name defense-in-depth the other packs use. A rejected selector makes the
// caller fail closed to Unverifiable — never a silent broad (or empty) test run.
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
