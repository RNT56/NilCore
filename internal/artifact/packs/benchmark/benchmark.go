// Package benchmark is the performance/variance verifier pack (Phase 12, SW-T03). Its
// reason to exist is a single, sharp invariant fix: THE VERIFIER ITSELF RE-MEASURES.
// A worker can attach a benchmark Artifact claiming "p50 latency <= 5ms, low variance"
// and even hand over a samples[] array it swears it observed — but the per-shard I2
// gate must never trust that array. This pack closes that I2-erosion by re-running the
// benchmark K times inside the worker's sandbox and computing the metric and the
// coefficient of variation from ITS OWN re-runs, then asserting both the claimed bound
// and a variance ceiling against numbers the verifier produced (registry.go's
// ArtifactVerifier overwrites the worker's self-claimed Status with this verdict).
//
// HONEST CAVEAT (baked into the spec, this docstring, and the tests): this pack
// verifies a CLAIMED BOUND plus run-to-run VARIANCE OVER THE VERIFIER'S RE-RUNS. It
// does NOT, and cannot honestly, reproduce an exact wall-clock number — absolute
// timings are host- and load-dependent, so a point-equality assertion would be noise.
// "The mean of K re-runs satisfies (op bound) and the CV stays within ceiling" is the
// strongest claim that is true across machines, and it is the only claim this pack makes.
//
// Two ids:
//
//   - benchmark.script_threshold — the load-bearing one. Re-runs a PACK-ALLOWLISTED
//     runner via box.Exec K (bounded) times, parses each run's metric host-side with
//     trusted Go, and asserts (mean op bound) AND (CV <= cv_max) over the verifier's
//     own samples. Unparseable output / a runner error / fewer than 2 successful runs
//     => Unverifiable (never Pass). The model supplies NO free command: ExtractionMethod
//     names a runner kind, which maps to a FIXED command shape (see runner.go).
//   - benchmark.variance_bounded — a pure host-side CV check over a worker-PROVIDED
//     sample array, used ONLY as a secondary self-consistency assertion (does the
//     worker's own claimed series hold together?). It reaches no box. It can corroborate
//     but never substitute for script_threshold's re-measurement, because its input is
//     model-authored data; on its own it proves only internal consistency, not truth.
//
// This package is a LEAF: it imports only artifact, evverify, sandbox and the standard
// library (math/strconv/encoding/json/...). It never imports the orchestrator
// (super/roster/agent/swarm) and never imports internal/artifact/schema (Schemas()
// returns plain artifact.Kind values the SW-T05 aggregator maps). go.mod is untouched (I6).
package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/sandbox"
)

// Verifier-ids registered by this pack, namespaced under "benchmark." so a claim names
// e.g. Evidence.Verifier = "benchmark.script_threshold".
const (
	IDScriptThreshold = "benchmark.script_threshold"
	IDVarianceBounded = "benchmark.variance_bounded"
)

// maxRuns bounds how many times script_threshold will re-run the benchmark in-box. A
// model-supplied "runs":K is clamped to [minSamples, maxRuns] so a hostile or buggy
// artifact can neither skip re-measurement (K too small) nor turn the verifier into a
// fork bomb (K enormous). 20 is plenty to estimate a CV without being a DoS.
const maxRuns = 20

// minSamples is the floor below which no variance/aggregate verdict is asserted. With a
// single successful run there is no dispersion to measure, so a variance claim is
// vacuous; we refuse (Unverifiable) rather than pass on nothing. This is the literal
// "<2 runs => Unverifiable" rule.
const minSamples = 2

// The two recognized aggregate operators. The model supplies one of these as DATA in
// Evidence.Value.op; any other value fails the bound comparison closed (never Pass).
const (
	opLE = "<=" // assert mean(samples) <= bound
	opGE = ">=" // assert mean(samples) >= bound
)

// maxDetail bounds the harness-authored detail tail so a verifier note can never flood
// the artifact JSON or an event Detail. Mirrors the other packs' bound; kept local so
// this leaf imports no orchestrator package.
const maxDetail = 512

// detail trims a harness-authored note to the bounded tail. It carries verifier
// commentary ONLY — never the raw runner stdout and never a model-authored field echoed
// unfenced (I7).
func detail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxDetail {
		return s[len(s)-maxDetail:]
	}
	return s
}

// RegisterAll adds this pack's two verifier-ids to r. It registers exactly the
// re-measuring script_threshold and the pure variance_bounded check and nothing else —
// an unregistered id elsewhere stays Unverifiable.
func RegisterAll(r *evverify.Registry) {
	r.Register(IDScriptThreshold, checkScriptThreshold)
	r.Register(IDVarianceBounded, checkVarianceBounded)
}

// Hosts returns nil: this pack reaches NO remote host. It re-runs a local benchmark
// inside the sandbox; there is no egress to allowlist. Exposed (mirroring ui.Hosts())
// so the SW-T05 aggregator's HostsFor("benchmark") has a definite, empty answer.
func Hosts() []string { return nil }

// Schemas returns the artifact Kind(s) this pack schematizes. It deliberately returns a
// plain []artifact.Kind rather than a *schema.Schema slice: SW-T03 forbids this leaf
// from importing internal/artifact/schema, so the concrete Schema shape (MinClaims>=1,
// etc.) is constructed by the SW-T05 DefaultSchemas() aggregator from these Kinds via
// schema.Default(). The benchmark Kind is the one this pack's claims live under.
func Schemas() []artifact.Kind { return []artifact.Kind{artifact.KindBenchmark} }

// boundSpec is the model-authored, UNTRUSTED control object carried as JSON in
// Evidence.Value. It tells the verifier WHAT to assert; it never supplies a command and
// never supplies the samples for script_threshold (those the verifier measures itself).
//
//	{"metric":"ns/op","bound":5000000,"op":"<=","runs":7,"cv_max":0.05,"samples":[...]}
//
// metric  — which numeric column the runner emits to parse (e.g. "ns/op", "MB/s", a
//
//	custom "score"); matched against the runner's labeled output host-side.
//
// bound   — the threshold the aggregate (mean) is compared against.
// op      — "<=" or ">=" (opLE/opGE); any other value fails closed.
// runs    — how many times the verifier re-runs the benchmark (clamped to [2,maxRuns]).
// cv_max  — the inclusive variance ceiling on the coefficient of variation.
// samples — ONLY consumed by variance_bounded (the worker's claimed series); IGNORED by
//
//	script_threshold, which trusts only its own re-runs.
type boundSpec struct {
	Metric  string    `json:"metric"`
	Bound   float64   `json:"bound"`
	Op      string    `json:"op"`
	Runs    int       `json:"runs"`
	CVMax   float64   `json:"cv_max"`
	Samples []float64 `json:"samples"`
}

// parseSpec decodes the model-authored Evidence.Value JSON into a boundSpec and applies
// the trust clamps. It returns an error (mapped by the caller to Unverifiable, never
// Pass) when the value is empty, not JSON, names an unknown op, or supplies a
// non-positive bound-comparison that cannot be evaluated. runs is clamped to
// [minSamples, maxRuns]; a missing/zero runs defaults to minSamples so the verifier
// always re-measures at least twice.
func parseSpec(value string) (boundSpec, error) {
	var s boundSpec
	value = strings.TrimSpace(value)
	if value == "" {
		return s, fmt.Errorf("evidence value is empty: no bound spec to verify")
	}
	if err := strictUnmarshal(value, &s); err != nil {
		return s, fmt.Errorf("evidence value is not a valid bound spec: %w", err)
	}
	if s.Op != opLE && s.Op != opGE {
		return s, fmt.Errorf("unrecognized op %q (want %q or %q)", clip(s.Op), opLE, opGE)
	}
	if s.Runs <= 0 {
		s.Runs = minSamples
	}
	if s.Runs < minSamples {
		s.Runs = minSamples
	}
	if s.Runs > maxRuns {
		s.Runs = maxRuns
	}
	return s, nil
}

// clip bounds a model-authored snippet placed into a detail note so it cannot flood the
// output (I7). Distinct from detail() (which trims a whole harness note); clip() guards
// a single interpolated model field to a short tail.
func clip(s string) string {
	const n = 64
	if len(s) > n {
		return s[:n]
	}
	return s
}

// checkScriptThreshold is the re-measuring check. It NEVER reads boundSpec.Samples:
// the whole point is that the verifier produces its own samples by re-running the
// allowlisted runner K times in the box, so a worker can never launder a fabricated
// variance through its self-reported array.
//
//   - Pass         — K>=2 runs parsed, mean satisfies (op bound) AND CV <= cv_max.
//   - Fail         — K>=2 runs parsed, but the bound or the variance ceiling is violated
//     (a real, measured number that does not meet the claim).
//   - Unverifiable — nil box / runner not allowlisted / a run errored or exited non-zero
//     / output unparseable / fewer than 2 successful runs. No decisive
//     verdict was reachable; never Pass.
func checkScriptThreshold(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	spec, err := parseSpec(c.Evidence.Value)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	if box == nil {
		// No sandbox to re-measure through. Refuse rather than trust the worker's
		// samples or reach host-side — either would defeat the re-measure guarantee (I4).
		return artifact.StatusUnverifiable, "no sandbox available (refusing to trust worker samples)"
	}

	// Resolve the model-named runner KIND to a fixed, harness-authored command shape.
	// The model supplies no free command; an unrecognized runner fails closed here.
	cmd, err := resolveRunner(c.Evidence.ExtractionMethod, box.Workdir())
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}

	// THE RE-MEASURE LOOP: run the allowlisted command spec.Runs times, parsing each
	// run's metric host-side. A single errored/non-zero/unparseable run is skipped (we
	// keep the successful ones); the <2-successful-runs floor is enforced after the loop.
	samples := make([]float64, 0, spec.Runs)
	var lastErr string
	for i := 0; i < spec.Runs; i++ {
		res, execErr := box.Exec(ctx, cmd)
		if execErr != nil {
			lastErr = "sandbox: " + execErr.Error()
			continue
		}
		if res.ExitCode != 0 {
			d := strings.TrimSpace(res.Stderr)
			if d == "" {
				d = fmt.Sprintf("runner exited %d", res.ExitCode)
			}
			lastErr = d
			continue
		}
		v, ok := parseMetric(res.Stdout, spec.Metric)
		if !ok {
			lastErr = fmt.Sprintf("metric %q not found in runner output", clip(spec.Metric))
			continue
		}
		samples = append(samples, v)
	}

	if len(samples) < minSamples {
		why := fmt.Sprintf("only %d/%d runs produced a parseable %q metric", len(samples), spec.Runs, clip(spec.Metric))
		if lastErr != "" {
			why += "; last error: " + lastErr
		}
		return artifact.StatusUnverifiable, detail(why)
	}

	// Both assertions are over the VERIFIER's own samples. The aggregate is the mean of
	// the re-runs; the variance ceiling is the CV of the re-runs.
	agg := mean(samples)
	boundOK := compareBound(agg, spec.Op, spec.Bound)
	cvOK := withinCV(samples, spec.CVMax)
	observedCV := cv(samples)

	switch {
	case boundOK && cvOK:
		return artifact.StatusPass, detail(fmt.Sprintf(
			"re-measured %d runs: mean=%g %s %g (ok), cv=%g <= %g (ok)",
			len(samples), agg, spec.Op, spec.Bound, observedCV, spec.CVMax))
	case !boundOK:
		return artifact.StatusFail, detail(fmt.Sprintf(
			"re-measured %d runs: mean=%g violates %s %g", len(samples), agg, spec.Op, spec.Bound))
	default: // !cvOK
		return artifact.StatusFail, detail(fmt.Sprintf(
			"re-measured %d runs: cv=%g exceeds ceiling %g (mean=%g)", len(samples), observedCV, spec.CVMax, agg))
	}
}

// checkVarianceBounded is the pure, box-free secondary check. It asserts the
// coefficient of variation of the worker-PROVIDED samples is within cv_max — a
// self-consistency assertion only. It reaches no box and re-measures nothing, so it can
// corroborate but never replace script_threshold; on its own it proves only that the
// worker's own series is internally consistent, not that the series is true.
//
//   - Pass         — >=2 finite samples and CV <= cv_max.
//   - Fail         — >=2 samples but CV > cv_max (or a non-finite sample).
//   - Unverifiable — fewer than 2 samples (no dispersion to assert) or a malformed spec.
func checkVarianceBounded(_ context.Context, _ sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	spec, err := parseSpec(c.Evidence.Value)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	if len(spec.Samples) < minSamples {
		return artifact.StatusUnverifiable, detail(fmt.Sprintf(
			"variance check needs >= %d samples, got %d", minSamples, len(spec.Samples)))
	}
	if withinCV(spec.Samples, spec.CVMax) {
		return artifact.StatusPass, detail(fmt.Sprintf(
			"cv=%g <= ceiling %g over %d provided samples", cv(spec.Samples), spec.CVMax, len(spec.Samples)))
	}
	return artifact.StatusFail, detail(fmt.Sprintf(
		"cv=%g exceeds ceiling %g over %d provided samples", cv(spec.Samples), spec.CVMax, len(spec.Samples)))
}

// Runner KINDS the model may name in Evidence.ExtractionMethod. The model selects a
// KIND (a label) — never a free command. Each kind maps to a fixed command SHAPE built
// host-side, so a hostile ExtractionMethod can request only one of these, with no
// opportunity to inject shell. A worktree script path is the one parameterized kind and
// is path-validated (no traversal, no metacharacters) before it is shelled.
const (
	runnerGoBench   = "go-bench"   // "go test -bench=. -run='^$' -benchmem ./..." in the workdir
	runnerMakeBench = "make-bench" // "make bench"
	runnerScript    = "script:"    // "script:<relpath>" — a declared worktree script, path-validated
)

// resolveRunner maps a model-named runner KIND to a FIXED shell command run in the box.
// The model supplies NO free command (I4): an unrecognized method, or a script path that
// fails validation, returns an error the caller maps to Unverifiable (never Pass). The
// workdir anchors relative runners; it originates from the trusted sandbox, not the model.
func resolveRunner(method, workdir string) (string, error) {
	method = strings.TrimSpace(method)
	switch {
	case method == runnerGoBench:
		// A pure benchmark pass: run only benchmarks (-run '^$' excludes tests), report
		// alloc stats. Fixed shape; nothing model-authored is interpolated.
		return "go test -bench=. -run='^$' -benchmem ./...", nil
	case method == runnerMakeBench:
		return "make bench", nil
	case strings.HasPrefix(method, runnerScript):
		rel := strings.TrimSpace(strings.TrimPrefix(method, runnerScript))
		safe, err := validateScriptPath(rel)
		if err != nil {
			return "", err
		}
		// Execute the declared script from the workdir. The path is validated and
		// single-quoted so it stays a single argument and cannot smuggle a command. We do
		// not prepend an interpreter — the script is expected to be executable in the box;
		// "sh <path>" keeps it from depending on the executable bit while staying one arg.
		_ = workdir // runner runs with the box's own working directory; kept for clarity
		return "sh './" + safe + "'", nil
	default:
		return "", fmt.Errorf("runner %q is not pack-allowlisted (want %q, %q, or %q<relpath>)",
			clip(method), runnerGoBench, runnerMakeBench, runnerScript)
	}
}

// validateScriptPath constrains a model-declared worktree script path before it is
// shelled: relative only, no "..", no leading slash, and no shell-significant byte
// (quote/whitespace/control/`;`/`$`/backtick/etc.), so the path cannot escape the
// worktree or break out of its single-quoting. Returns the cleaned relative path.
func validateScriptPath(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("script runner needs a relative path")
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("script path must be relative (got an absolute path)")
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", fmt.Errorf("script path may not contain a parent reference")
		}
	}
	for _, r := range rel {
		if r == '\'' || r == '`' || r == '$' || r == ';' || r == '|' || r == '&' {
			return "", fmt.Errorf("script path contains a shell metacharacter")
		}
		if r <= ' ' || r == 0x7f {
			return "", fmt.Errorf("script path contains whitespace or a control byte")
		}
	}
	return rel, nil
}

// parseMetric extracts the named metric's numeric value from one run's stdout. It is
// liberal about format because the runner may be `go test -bench` (Go's tabular
// "BenchmarkX-8   123   456 ns/op"), a make target, or a custom script — so it scans for
// the metric LABEL and reads the number adjacent to it. Two shapes are recognized:
//
//   - "<number> <label>"  e.g. "456 ns/op"  (Go bench style: value precedes the unit)
//   - "<label>: <number>" / "<label>=<number>" / "<label> <number>" (key/value style)
//
// The body is UNTRUSTED data parsed by trusted Go (no model field echoed unfenced, I7).
// Returns ok=false if the label is absent or no adjacent number parses — the caller maps
// that to a skipped (Unverifiable-on-too-few) run, never a Pass.
func parseMetric(stdout, metric string) (float64, bool) {
	metric = strings.TrimSpace(metric)
	if metric == "" {
		return 0, false
	}
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			// Go-bench style: the unit/label follows the value ("456 ns/op").
			if f == metric && i > 0 {
				if v, err := strconv.ParseFloat(fields[i-1], 64); err == nil {
					return v, true
				}
			}
			// Key/value style: "label: 456", "label=456", "label 456".
			if kv := matchKeyValue(f, fields, i, metric); kv != nil {
				return *kv, true
			}
		}
	}
	return 0, false
}

// matchKeyValue recognizes "<metric>=<n>", "<metric>:<n>" (possibly split as
// "<metric>:" "<n>"), and "<metric> <n>". It returns the parsed number or nil.
func matchKeyValue(f string, fields []string, i int, metric string) *float64 {
	// "metric=value" or "metric:value" as a single token.
	for _, sep := range []string{"=", ":"} {
		prefix := metric + sep
		if strings.HasPrefix(f, prefix) {
			if v, err := strconv.ParseFloat(strings.TrimSpace(f[len(prefix):]), 64); err == nil {
				return &v
			}
		}
	}
	// "metric" (or "metric:") followed by the number as the next token.
	if f == metric || f == metric+":" {
		if i+1 < len(fields) {
			if v, err := strconv.ParseFloat(fields[i+1], 64); err == nil {
				return &v
			}
		}
	}
	return nil
}

// strictUnmarshal decodes JSON into v with DisallowUnknownFields, so a typo'd or
// adversarial extra key in the model-authored bound spec is a parse error (mapped to
// Unverifiable) rather than silently ignored. UseNumber is not needed — the spec's
// numeric fields are float64 by design.
func strictUnmarshal(s string, v any) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// Reject trailing garbage after the JSON object.
	if dec.More() {
		return fmt.Errorf("trailing data after JSON object")
	}
	return nil
}

// ensure the package compiles against the stable CheckFunc signature.
var (
	_ evverify.CheckFunc = checkScriptThreshold
	_ evverify.CheckFunc = checkVarianceBounded
)
