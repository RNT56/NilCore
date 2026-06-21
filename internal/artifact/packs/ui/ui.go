// Package ui is the UI/browser domain verifier pack (Pillar 2, P11-T10). It
// registers three namespaced verifier-ids — ui.flow_passes, ui.no_console_errors,
// ui.screenshot_captured — each of which drives the in-sandbox nilcore-browser CDP
// driver via a SINGLE box.Exec and asserts a typed Status over the driver's
// browserwire.Observation JSON.
//
// WHY this stays a typed check and not "generic browsing" (the NON-GOAL guard):
// every id reduces to one runnable assertion the harness makes — a substring must
// appear after a flow, the console must be empty, a screenshot must be captured —
// never a vacuous "the page loaded, looks fine". An empty target (e.g. an empty
// Evidence.Value for a flow) is Unverifiable, never a free Pass.
//
// Invariant compliance:
//   - I4: every browser reach is one box.Exec of the nilcore-browser driver; a nil
//     box, or a driver that exits non-zero, fails closed to Unverifiable — there is
//     no host-side browser fallback. Model-supplied flow actions are single-quoted
//     via browserwire.ShellSingleQuote (the shared, tested boundary) so they stay
//     DATA and can never become a second command.
//   - I6: stdlib only (encoding/json + fmt + strings) plus the in-box driver; no
//     CDP/headless module — go.mod is untouched.
//   - I7: the driver's stdout Observation is parsed host-side as trusted Go (data,
//     not instructions); only a bounded, harness-authored detail tail leaves the
//     pack. A model-authored Evidence.Value is matched against, never echoed as an
//     instruction.
//
// This package is a LEAF: it imports only artifact, evverify, sandbox, worktreefs
// (transitively, via evverify) and browserwire — never internal/tools, never
// internal/model, never the orchestrator. go list -deps asserts this.
package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/browserwire"
	"nilcore/internal/sandbox"
)

// defaultBrowserDriver is the in-sandbox CDP driver binary baked into the sandbox
// image (cmd/tools/nilcore-browser). It is invoked by name; the box (not this
// package) resolves and confines it (I4). Kept identical to the tool's default so
// the verifier drives the same driver the model's browser_view tool does.
const defaultBrowserDriver = "nilcore-browser"

// maxActionsBytes bounds the model-authored flow actions payload placed into the
// driver command, mirroring the browser tool's cap so an oversized blob is refused
// rather than shelled.
const maxActionsBytes = 64 << 10

// RegisterAll registers the three UI verifier-ids onto r. Calling it makes exactly
// ui.flow_passes, ui.no_console_errors, and ui.screenshot_captured Lookup-able;
// without it (packs off) those ids resolve to Unverifiable, never Pass.
func RegisterAll(r *evverify.Registry) {
	r.Register("ui.flow_passes", checkFlowPasses)
	r.Register("ui.no_console_errors", checkNoConsoleErrors)
	r.Register("ui.screenshot_captured", checkScreenshotCaptured)
	r.Register("ui.value_present", checkValuePresent)
}

// Hosts is the documented egress host-set this pack reaches. A UI flow targets the
// site under test, whose host is supplied per-claim (the flow's --url / navigate
// steps), so there is no fixed catalog of remote hosts to allowlist here: the pack
// reaches only whatever the role's egress profile already permits, and a denied
// host makes the driver exit non-zero ⇒ Unverifiable (fail-closed). Exposed so the
// packs aggregator's HostsFor("ui") (P11-T11) has a definite, if empty, answer that
// the P11-T35 cross-check can assert against.
func Hosts() []string { return nil }

// drive runs the nilcore-browser driver once for the given (already shell-safe)
// argument tail and parses its stdout Observation. It centralizes the fail-closed
// discipline every check shares: nil box ⇒ refuse (no host-side browser), a sandbox
// error or a non-zero driver exit or unparseable stdout ⇒ Unverifiable with a
// bounded detail. On success it returns the parsed Observation; ok is false on any
// fail-closed path (the caller returns the carried Status/detail verbatim).
func drive(ctx context.Context, box sandbox.Sandbox, argTail string) (browserwire.Observation, artifact.Status, string, bool) {
	if box == nil {
		// Refuse rather than reach for a host-side browser, which would bypass the
		// sandbox boundary and the egress allowlist (I4).
		return browserwire.Observation{}, artifact.StatusUnverifiable, "no sandbox available (refusing a host-side browser)", false
	}

	driver := defaultBrowserDriver
	cmd := driver + " " + argTail + " --format json"

	res, err := box.Exec(ctx, cmd)
	if err != nil {
		// The box could not run the command at all — not a decisive verdict, fail closed.
		return browserwire.Observation{}, artifact.StatusUnverifiable, clip("sandbox: " + err.Error()), false
	}
	if res.ExitCode != 0 {
		// A driver non-zero exit is an unreachable/denied host, a missing browser binary,
		// or a timeout — never a fabricated Pass. Fail closed to Unverifiable.
		d := strings.TrimSpace(res.Stderr)
		if d == "" {
			d = fmt.Sprintf("%s exited %d", driver, res.ExitCode)
		}
		return browserwire.Observation{}, artifact.StatusUnverifiable, clip(d), false
	}

	var obs browserwire.Observation
	if err := json.Unmarshal([]byte(res.Stdout), &obs); err != nil {
		// Unparseable driver output: we cannot read a verdict from it — Unverifiable,
		// never a guess. The raw body is NOT echoed (I7); only a bounded note.
		return browserwire.Observation{}, artifact.StatusUnverifiable, "unparseable driver observation", false
	}
	return obs, artifact.StatusPass, "", true
}

// flowURLArg validates and single-quotes an optional leading --url for a flow, so a
// model-authored navigation seed cannot break out of the quoting. An empty URL is
// allowed (the flow's own navigate steps drive it); a malformed one is rejected.
func flowURLArg(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	for _, r := range raw {
		if r == '\'' {
			return "", fmt.Errorf("url may not contain a single quote")
		}
		if r <= ' ' || r == 0x7f {
			return "", fmt.Errorf("url may not contain whitespace or control characters")
		}
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return "", fmt.Errorf("only http and https url is allowed")
	}
	return " --url " + browserwire.ShellSingleQuote(raw), nil
}

// checkFlowPasses drives a model-supplied flow (the actions JSON in Evidence.Value
// — quoted as DATA via ShellSingleQuote) and asserts that the claim's expected
// substring appears in the resulting page title or text. The substring under test
// is the claim's Field-labeled assertion carried in Statement when present, falling
// back to a marker the worker embeds; an EMPTY assertion target ⇒ Unverifiable
// (never a vacuous Pass — the NON-GOAL guard).
//
// Evidence.Value carries the flow actions JSON (the steps to replay). The expected
// post-flow substring is Evidence.ExtractionMethod (the worker states "after this
// flow, expect to see X"); both are model-authored DATA the harness asserts over.
func checkFlowPasses(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	actions := strings.TrimSpace(c.Evidence.Value)
	if actions == "" {
		// No flow to replay: there is nothing to assert. Refuse rather than Pass on a
		// page that was never driven (NON-GOAL: not generic browsing).
		return artifact.StatusUnverifiable, "empty flow actions: nothing to verify"
	}
	if len(actions) > maxActionsBytes {
		return artifact.StatusUnverifiable, fmt.Sprintf("flow actions too large (%d > %d bytes)", len(actions), maxActionsBytes)
	}
	expect := strings.TrimSpace(c.Evidence.ExtractionMethod)
	if expect == "" {
		// No expected outcome to assert against ⇒ the flow would "pass" vacuously. Refuse.
		return artifact.StatusUnverifiable, "empty expected substring: a flow check must assert an outcome"
	}

	urlArg, err := flowURLArg(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, clip(err.Error())
	}
	// ONE driver invocation: --actions <quoted JSON> [--url <quoted url>]. The actions
	// are single-quoted so quotes/`;`/`$()`/newlines inside a selector or typed text
	// stay DATA (I4); the driver replays them as CDP params, never shell.
	argTail := "--actions " + browserwire.ShellSingleQuote(actions) + urlArg
	obs, st, d, ok := drive(ctx, box, argTail)
	if !ok {
		return st, d
	}

	if substringPresent(obs, expect) {
		return artifact.StatusPass, "flow produced the expected outcome"
	}
	// The flow ran but the expected substring was not observed: the asserted outcome
	// is false ⇒ Fail (re-derive), distinct from a driver failure (Unverifiable).
	return artifact.StatusFail, "expected substring not present after flow"
}

// checkValuePresent is the EXTRACTION verifier-id (Phase 14): the browse agent
// recorded "I extracted Value from SourceURL"; this re-navigates to SourceURL
// INDEPENDENTLY (a fresh in-box driver run, never trusting the live session) and
// asserts the extracted Value substring is actually present in the page's title or
// text. A present value ⇒ Pass; absent after a successful render ⇒ Fail (re-derive);
// a driver failure / empty value / no source ⇒ Unverifiable (never a vacuous Pass).
// This closes the I2 loop for browse extraction: a finding ships GREEN only because
// the harness re-derived it from the source, not because the agent reported it.
func checkValuePresent(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	want := strings.TrimSpace(c.Evidence.Value)
	if want == "" {
		return artifact.StatusUnverifiable, "empty value: nothing to confirm at the source"
	}
	// Navigate to SourceURL directly (batch mode) — here Value is the extracted DATUM,
	// not flow actions, so we do NOT route it through navArgTail (which would mis-treat
	// it as an actions payload). A missing/invalid source ⇒ Unverifiable.
	urlArg, err := flowURLArg(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, clip(err.Error())
	}
	if urlArg == "" {
		return artifact.StatusUnverifiable, "no source_url to re-derive the value from"
	}
	obs, st, d, ok := drive(ctx, box, strings.TrimSpace(urlArg))
	if !ok {
		return st, d
	}
	if substringPresent(obs, want) {
		return artifact.StatusPass, "value present at source"
	}
	return artifact.StatusFail, "extracted value not present at source on re-derivation"
}

// checkNoConsoleErrors navigates to the claim's SourceURL (or replays its flow when
// Evidence.Value carries actions) and asserts the driver observed NO console
// messages. Console[] non-empty ⇒ Fail (the page logged errors); empty ⇒ Pass; a
// driver failure ⇒ Unverifiable.
func checkNoConsoleErrors(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	argTail, st, d, ok := navArgTail(c)
	if !ok {
		return st, d
	}
	obs, st, d, ok := drive(ctx, box, argTail)
	if !ok {
		return st, d
	}
	if len(obs.Console) == 0 {
		return artifact.StatusPass, "no console messages"
	}
	return artifact.StatusFail, fmt.Sprintf("%d console message(s) present", len(obs.Console))
}

// checkScreenshotCaptured navigates/replays and asserts the driver captured a
// screenshot (a non-empty ScreenshotB64). A captured screenshot ⇒ Pass; an empty
// capture over a successful driver run ⇒ Fail (the page rendered nothing to shoot);
// a driver failure ⇒ Unverifiable.
func checkScreenshotCaptured(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	argTail, st, d, ok := navArgTail(c)
	if !ok {
		return st, d
	}
	obs, st, d, ok := drive(ctx, box, argTail)
	if !ok {
		return st, d
	}
	if strings.TrimSpace(obs.ScreenshotB64) != "" {
		return artifact.StatusPass, "screenshot captured"
	}
	return artifact.StatusFail, "driver returned no screenshot"
}

// navArgTail builds the driver argument tail for a navigate-or-flow check (the
// no_console_errors / screenshot_captured shape): if Evidence.Value carries flow
// actions, replay them (quoted); otherwise navigate to the validated SourceURL. A
// claim with neither a flow nor a URL ⇒ Unverifiable (nothing to observe). ok is
// false on any refuse path, carrying the Status/detail to return verbatim.
func navArgTail(c artifact.Claim) (string, artifact.Status, string, bool) {
	actions := strings.TrimSpace(c.Evidence.Value)
	if actions != "" {
		if len(actions) > maxActionsBytes {
			return "", artifact.StatusUnverifiable, fmt.Sprintf("flow actions too large (%d > %d bytes)", len(actions), maxActionsBytes), false
		}
		urlArg, err := flowURLArg(c.Evidence.SourceURL)
		if err != nil {
			return "", artifact.StatusUnverifiable, clip(err.Error()), false
		}
		return "--actions " + browserwire.ShellSingleQuote(actions) + urlArg, artifact.StatusUnverified, "", true
	}
	// No flow: navigate to the source URL.
	urlArg, err := flowURLArg(c.Evidence.SourceURL)
	if err != nil {
		return "", artifact.StatusUnverifiable, clip(err.Error()), false
	}
	if urlArg == "" {
		return "", artifact.StatusUnverifiable, "no flow actions and no source_url: nothing to observe", false
	}
	// flowURLArg already prefixes " --url ..."; trim the leading space for a clean tail.
	return strings.TrimSpace(urlArg), artifact.StatusUnverified, "", true
}

// substringPresent reports whether a whitespace-normalized form of want appears in
// the observation's title or text. Both sides are normalized (collapsed internal
// whitespace, lowercased) so an incidental run of spaces or a case difference does
// not defeat a genuine match — but an empty want is never present (NON-GOAL guard).
func substringPresent(obs browserwire.Observation, want string) bool {
	w := normalize(want)
	if w == "" {
		return false
	}
	hay := normalize(obs.Title + " " + obs.Text)
	return strings.Contains(hay, w)
}

// normalize collapses runs of whitespace to a single space and lowercases, so the
// substring match is robust to incidental formatting differences between the
// model-authored expectation and the rendered page.
func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// clip bounds a harness-authored detail note so a driver's stderr tail can never
// flood the artifact JSON or an event Detail. It carries verifier commentary only,
// never the raw remote body or a model-authored field echoed unfenced.
const maxDetail = 512

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxDetail {
		return s[len(s)-maxDetail:]
	}
	return s
}
