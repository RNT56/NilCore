package trace

import (
	"fmt"
	"sort"
	"strings"
)

// why.go is the harness-authored derivation table: a PURE function from one
// event (its kind + metadata Detail) and the prior-step context to a (Title,
// Why) pair. This is the heart of the explorer — it is where raw log kinds
// become a legible causal story.
//
// Two rules govern every line here:
//
//   - I7 (metadata only): Title and Why are composed from KNOWN-SAFE metadata
//     fields (exit codes, pass/fail flags, counts, ids, branch names, the goal
//     string the harness itself recorded). We never interpolate a raw model or
//     tool body — those never reach the Detail map in the first place, and we do
//     not start trusting arbitrary free-text fields here.
//
//   - Grounded honesty: a Why is emitted ONLY when the surrounding events
//     actually justify it. "after 2 consecutive verify failures" is printed only
//     when the context counted two; otherwise Why stays empty rather than
//     inventing a plausible-sounding cause.

// ctx is the running causal context the builder threads through the event
// stream. It carries just enough recent history to ground a Why — never bodies,
// only counts and the last few outcomes. A new ctx is created per task.
type ctx struct {
	verifyFails  int    // consecutive verify failures not yet cleared by a pass
	lastVerifyOK *bool  // last verify outcome, nil if none yet
	sawAdvisor   bool   // an advisor consult happened since the last verify fail
	raceN        int    // candidates announced by a race_escalate, if any
	lastKind     string // the previous event's kind
}

// safeDetail copies only known, metadata-only keys out of the raw event Detail
// into the string map the rest of the package carries. Anything not on the
// allowlist is dropped — so even if an upstream bug stuffed a body into a novel
// field, it never reaches a Step (defence in depth for I7). Values are
// stringified positionally; the renderer fences them, so a stray newline cannot
// reshape the tree.
func safeDetail(d map[string]any) map[string]string {
	if len(d) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range d {
		if !safeKeys[k] {
			continue
		}
		out[k] = stringify(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// safeKeys is the allowlist of Detail fields the trace is permitted to surface.
// Every entry is a harness-written scalar metadatum observed in the event
// emitters (model_call step/stop/out_tokens; tool_exec cli/cmd/tool/exit;
// verify passed; gate action/class/allowed; race n; advisor calls; integrate
// branch/sha; project iteration/unmet; etc.). Free-text body fields are
// deliberately absent. "cmd" is a command string the harness recorded for its
// own tool; it is metadata about the run, but we still fence it on render.
var safeKeys = map[string]bool{
	"step": true, "stop": true, "out_tokens": true,
	"cli": true, "cmd": true, "tool": true, "exit": true, "name": true,
	"passed": true, "action": true, "class": true, "allowed": true,
	"n": true, "calls": true,
	"branch": true, "sha": true, "pre_sha": true, "base_repo": true,
	"iteration": true, "unmet": true, "no_progress": true, "phase": true,
	"pass": true, "checked": true, "failed": true, "remaining": true,
	"escalate": true, "proposed": true, "added": true, "dropped": true,
	"total": true, "count": true,
}

// stringify renders a metadata scalar deterministically. Booleans and integers
// print plainly; a float that is integral prints without a decimal tail (JSON
// decodes every number into float64, so counts arrive as e.g. 2.0). A string is
// returned as-is (it is then fenced at render time).
func stringify(v any) string {
	switch x := v.(type) {
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case string:
		return x
	default:
		return fmt.Sprintf("%v", x)
	}
}

// detailStr fetches a stringified field, or "" if absent.
func detailStr(d map[string]any, key string) string {
	if d == nil {
		return ""
	}
	if v, ok := d[key]; ok {
		return stringify(v)
	}
	return ""
}

// detailBool fetches a boolean field; ok reports whether it was present.
func detailBool(d map[string]any, key string) (val, ok bool) {
	if d == nil {
		return false, false
	}
	v, present := d[key]
	if !present {
		return false, false
	}
	b, isBool := v.(bool)
	return b, isBool
}

// annotate is the derivation table proper: given a kind, its raw Detail, and the
// running context, it returns the (Title, Why) for the node and advances the
// context. The switch is grouped by causal family; each arm explains the kind in
// operator language and, where the surrounding events justify it, links cause to
// effect.
func annotate(kind string, d map[string]any, c *ctx) (title, why string) {
	switch {
	// --- task lifecycle -------------------------------------------------------
	case kind == "task_start":
		title = "task started"
		// The goal is harness-recorded metadata about intent; show it fenced.
		if g := detailStr(d, "goal"); g != "" {
			why = "goal: " + g
		}

	// --- a thinking turn ------------------------------------------------------
	case kind == "model_call":
		title = "model turn"
		if s := detailStr(d, "stop"); s != "" {
			title = "model turn (" + s + ")"
		}
		// A model turn right after a red verify is the recovery attempt.
		if c.lastVerifyOK != nil && !*c.lastVerifyOK {
			why = recoveryWhy(c)
		}

	// --- a tool execution -----------------------------------------------------
	case kind == "tool_exec":
		title, why = toolTitle(d)

	// --- the verifier, the authority on "done" --------------------------------
	case strings.HasSuffix(kind, "verify") || kind == "verify":
		passed, ok := detailBool(d, "passed")
		switch {
		case ok && passed:
			title = verifyLabel(kind) + " PASSED"
			if c.verifyFails > 0 {
				why = fmt.Sprintf("green after %s", plural(c.verifyFails, "failed attempt"))
			}
		case ok && !passed:
			title = verifyLabel(kind) + " FAILED"
			why = "the project's checks did not pass — this gates the run, not the backend's self-report"
		default:
			title = verifyLabel(kind)
		}

	// --- the human gate on irreversible actions -------------------------------
	case kind == "gate":
		allowed, _ := detailBool(d, "allowed")
		act := detailStr(d, "action")
		if act == "" {
			act = "an irreversible action"
		}
		verdict := "DENIED"
		if allowed {
			verdict = "approved"
		}
		title = "human gate: " + verdict
		why = fmt.Sprintf("%s is irreversible (class %s), so it required human sign-off",
			act, fallback(detailStr(d, "class"), "irreversible"))

	// --- advisor consult: the recovery escalation -----------------------------
	case kind == "advisor_consult", kind == "advisor":
		title = "consulted the advisor"
		if c.lastVerifyOK != nil && !*c.lastVerifyOK {
			why = "to recover after " + plural(c.verifyFails, "failed verify") + " — asked the advisor for a way forward"
		} else {
			why = "escalated to the advisor for guidance"
		}

	// --- explicit escalation paths --------------------------------------------
	case kind == "escalate" || strings.HasPrefix(kind, "race_escalate"):
		title = "escalated"
		if n := detailStr(d, "n"); n != "" {
			why = "racing " + n + " backends after the single attempt did not go green"
		} else {
			why = "the cheaper path did not succeed, so the run escalated"
		}

	// --- a raced outcome (one candidate of a cluster) -------------------------
	case kind == "race_outcome":
		passed, _ := detailBool(d, "passed")
		if passed {
			title = "race candidate PASSED the verifier"
			why = "the verifier accepted this backend's result — it wins the race"
		} else {
			title = "race candidate did not pass"
			why = "the verifier rejected this backend's result"
		}

	// --- integration / merge --------------------------------------------------
	case kind == "integration_merge", kind == "integrate":
		title = "integrated branch"
		if b := detailStr(d, "branch"); b != "" {
			title = "integrated branch " + b
		}
		why = "the branch verified green, so its work was merged in"
	case kind == "integration_verify":
		passed, _ := detailBool(d, "passed")
		if passed {
			title = "integration re-verify PASSED"
			why = "the merge result was re-checked before it was kept"
		} else {
			title = "integration re-verify FAILED"
			why = "the merge did not hold up under the checks"
		}
	case kind == "integration_rollback":
		title = "rolled back the merge"
		why = "the integrated branch failed re-verification, so the merge was reverted"
	case kind == "integration_conflict":
		title = "merge conflict"
		why = "the branch could not be merged cleanly and needs attention"

	// --- project / supervisor lifecycle (coarse, metadata-only) ---------------
	case kind == "project_done":
		title = "project complete"
	case kind == "project_verify":
		title = "project re-verified"
		if u := detailStr(d, "unmet"); u != "" && u != "0" {
			why = u + " acceptance criteria still unmet"
		}
	case strings.HasPrefix(kind, "project_"):
		title = "project: " + humanTail(kind, "project_")
	case strings.HasPrefix(kind, "super_"):
		title = "supervisor: " + humanTail(kind, "super_")
	case strings.HasPrefix(kind, "subagent_"):
		title = "subagent: " + humanTail(kind, "subagent_")

	// --- safety trips ---------------------------------------------------------
	case kind == "unauthorized_command" || kind == "unauthorized_gate" ||
		kind == "tool_denied" || kind == "spawn_denied" || kind == "injection_flagged":
		title = "blocked: " + strings.ReplaceAll(kind, "_", " ")
		why = "a safety boundary refused this — the action was not permitted"

	// --- fallback: a known kind we have no special story for ------------------
	default:
		title = strings.ReplaceAll(kind, "_", " ")
	}

	// Advance the running context AFTER deriving (so a verify's Why can see the
	// fail count it is clearing).
	advance(kind, d, c)
	return title, why
}

// recoveryWhy explains a model turn that follows a red verify, naming the
// advisor when one was consulted in between.
func recoveryWhy(c *ctx) string {
	if c.sawAdvisor {
		return "re-planning after the failed verify, with the advisor's guidance"
	}
	return "re-attempting because the last verify was red"
}

// toolTitle derives a tool node's label from whichever metadata field the
// emitter used (a delegated CLI, a sandboxed shell command, or a native
// structured tool), plus its exit code where present.
func toolTitle(d map[string]any) (title, why string) {
	switch {
	case detailStr(d, "tool") != "":
		title = "ran tool: " + detailStr(d, "tool")
	case detailStr(d, "cli") != "":
		title = "ran CLI backend: " + detailStr(d, "cli")
	case detailStr(d, "cmd") != "":
		// cmd is a harness-recorded command string — metadata about the run, but
		// fenced on render. Keep the label generic; the command sits in Detail.
		title = "ran a sandboxed command"
	default:
		title = "ran a tool"
	}
	if ex := detailStr(d, "exit"); ex != "" {
		if ex == "0" {
			why = "exited 0 (success)"
		} else {
			why = "exited " + ex + " (non-zero — a result, not a crash)"
		}
	}
	return title, why
}

// verifyLabel names the kind of verify (final, integration, artifact, …) so the
// title reads naturally.
func verifyLabel(kind string) string {
	switch kind {
	case "verify":
		return "verify"
	case "final_verify":
		return "final verify"
	case "integration_verify":
		return "integration verify"
	case "artifact_verify":
		return "artifact verify"
	case "project_verify":
		return "project verify"
	case "schema_verify":
		return "schema verify"
	default:
		return strings.ReplaceAll(kind, "_", " ")
	}
}

// advance threads the causal context forward over one event.
func advance(kind string, d map[string]any, c *ctx) {
	c.lastKind = kind
	switch {
	case kind == "advisor_consult" || kind == "advisor":
		c.sawAdvisor = true
	case kind == "race_escalate":
		if n := detailStr(d, "n"); n != "" {
			_, _ = fmt.Sscanf(n, "%d", &c.raceN)
		}
	case strings.HasSuffix(kind, "verify") || kind == "verify":
		if passed, ok := detailBool(d, "passed"); ok {
			c.lastVerifyOK = boolPtr(passed)
			if passed {
				c.verifyFails = 0
				c.sawAdvisor = false
			} else {
				c.verifyFails++
			}
		}
	}
}

// --- small helpers ----------------------------------------------------------

func boolPtr(b bool) *bool { return &b }

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// humanTail turns a "family_rest_of_kind" into "rest of kind" for the coarse
// project_/super_/subagent_ arms.
func humanTail(kind, prefix string) string {
	return strings.ReplaceAll(strings.TrimPrefix(kind, prefix), "_", " ")
}

// sortedKeys returns a Detail map's keys in stable order, so render output is
// deterministic.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
