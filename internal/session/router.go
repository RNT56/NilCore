package session

// router.go implements the auto-router (C2-T02): the SupervisorFirstRouter that
// decides which machine runs a NEW (non-in-flight) drive from the principal's
// message and the bounded WorkState. The Route enum and the Router interface live
// in state.go (declared by C2-T01); this file is the concrete classifier.
//
// One cheap, METERED classifier call (JSON-out, ~256 tokens) proposes a route; it
// is parsed with the same defensive firstText + brace-extraction pattern that
// route.Review and summarize use, so a chatty or malformed reply never crashes the
// router. The classifier's native-vs-feature-vs-project sizing is reconciled with
// the existing agent.ShouldSupervise heuristic so that judgment stays the single
// source of truth: a "feature/project" proposal is only honored if ShouldSupervise
// agrees the goal is complex, and a "native" proposal is upgraded to supervise
// when ShouldSupervise says the goal warrants it. On unparseable output the router
// falls back to the pure-function ShouldSupervise over the goal text (no model
// call) — never silently RouteNative.
//
// RouteContinue (the persistence requirement) is detected locally, before the
// model call, when the message references the active goal carried in
// State.Summary.Goal. RouteChat answers meta questions ("what are you working
// on?") with no loop.
//
// Trust line (I7): the principal's own message is trusted input — it is NOT fenced
// as data for the classifier. Only tool/file/web content a classifier transitively
// reads would need fencing, and this router reads nothing but the principal text.

import (
	"context"
	"encoding/json"
	"strings"

	"nilcore/internal/eventlog"
	"nilcore/internal/model"
)

// SupervisorFirstRouter is the default auto-router. Classifier MUST be the
// conversation-metered provider (so the routing call counts against the
// conversation ceiling, §6); ShouldSupervise is the pure-function heuristic reused
// from the agent wiring (orchestrator.go) — the single source of the
// native-vs-supervise sizing judgment, and the no-model fallback on unparseable
// classifier output. Log is the append-only audit (nil-safe); a nil Log simply
// records nothing.
type SupervisorFirstRouter struct {
	Classifier      model.Provider         // the METERED provider (same conversation ledger)
	ShouldSupervise func(goal string) bool // reused agent heuristic; also the parse-failure fallback
	Log             *eventlog.Log          // metadata-only session_route audit; nil-safe
	ID              string                 // conversation id for the audit Task field (optional)
}

// classifierSys is the system prompt for the one cheap routing call. It asks for a
// compact JSON object naming the route and a short reason; the reason is logged by
// length only (never the body). The four work routes mirror the Route enum's
// machines; "continue" is decided locally (not by the model) so it is intentionally
// omitted from the model's choices.
const classifierSys = `You are a fast request classifier for a coding agent. Decide which machine should handle the user's request and reply with ONLY a JSON object:
{"route": string, "reason": string}
"route" is exactly one of:
- "chat"      — a meta or conversational question that needs no code change (e.g. "what are you working on?", "explain this", "why did you do that?").
- "native"    — a small, localized code change a single loop can finish (a typo, a one-file fix, a rename, a small function).
- "supervise" — a multi-step feature or change spanning several files that benefits from planning and sub-tasks.
- "project"   — building or scaffolding a whole project/service from little or nothing (multiple components, tests, packaging).
"reason" is one short sentence. Respond with ONLY the JSON object.`

// Route classifies one principal message into a Route. It first checks the local,
// no-model RouteContinue rule (does the message reference the active goal?), then
// makes one metered classifier call, parses it defensively, and reconciles the
// proposal with ShouldSupervise. On unparseable output it falls back to the pure
// ShouldSupervise function (never crashes, never silently RouteNative). A
// metadata-only session_route event records the decision and the reason length.
func (r *SupervisorFirstRouter) Route(ctx context.Context, text string, st WorkState) (Route, error) {
	// Local, no-model continue detection: a follow-up that references the active
	// goal continues the work rather than restarting it. Checked first so a cheap
	// "keep going" never spends a classifier call.
	if referencesGoal(text, st.Summary.Goal) {
		r.logRoute(RouteContinue, 0, false)
		return RouteContinue, nil
	}

	// One cheap metered classifier call. A transport error is returned to the
	// caller (the Session returns to Idle); it is NOT a parse failure.
	if r.Classifier == nil {
		// No classifier wired: degrade to the pure heuristic rather than crash.
		route := r.fallback(text)
		r.logRoute(route, 0, true)
		return route, nil
	}

	resp, err := r.Classifier.Complete(ctx, classifierSys,
		[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: text}}}}, nil, 256)
	if err != nil {
		return RouteContinue, err
	}

	proposed, reason, ok := parseRoute(firstTextBlock(resp.Content))
	if !ok {
		// Unparseable: fall back to the pure ShouldSupervise function over the goal
		// text — no second model call, never silently RouteNative.
		route := r.fallback(text)
		r.logRoute(route, 0, true)
		return route, nil
	}

	route := r.reconcile(proposed, text)
	r.logRoute(route, len(reason), false)
	return route, nil
}

// reconcile folds the classifier's proposal together with the ShouldSupervise
// heuristic so that the native-vs-supervise sizing stays the heuristic's single
// judgment. RouteChat is honored as-is (a meta answer needs no sizing). For the
// work routes:
//
//   - a "native" proposal is UPGRADED to RouteSupervise when ShouldSupervise judges
//     the goal complex (the heuristic overrules a too-small estimate);
//   - a "supervise"/"project" proposal is honored only if ShouldSupervise agrees the
//     goal is complex; if the heuristic says simple, it is DOWNGRADED to RouteNative
//     (the heuristic overrules a too-large estimate). "project" that survives the
//     heuristic stays RouteProject (whole-project is a project-loop concern the
//     classifier alone names).
//
// When ShouldSupervise is nil the proposal is taken at face value (no heuristic to
// reconcile against), so the router still works before the heuristic is wired.
func (r *SupervisorFirstRouter) reconcile(proposed Route, goal string) Route {
	if proposed == RouteChat {
		return RouteChat
	}
	complex := r.supervise(goal)
	switch proposed {
	case RouteNative:
		if complex {
			return RouteSupervise
		}
		return RouteNative
	case RouteSupervise:
		if complex {
			return RouteSupervise
		}
		return RouteNative
	case RouteProject:
		if complex {
			return RouteProject
		}
		return RouteNative
	default:
		// An unknown route value from parseRoute (shouldn't happen) is sized by the
		// heuristic, never blindly trusted.
		return r.fallback(goal)
	}
}

// fallback is the pure-function path taken when the classifier output is
// unparseable (or no classifier is wired): no model call, just the ShouldSupervise
// heuristic over the goal text. Complex ⇒ RouteSupervise, simple ⇒ RouteNative. It
// never returns RouteProject (whole-project sizing needs the classifier) and never
// silently defaults to RouteNative without consulting the heuristic.
func (r *SupervisorFirstRouter) fallback(goal string) Route {
	if r.supervise(goal) {
		return RouteSupervise
	}
	return RouteNative
}

// supervise consults the injected heuristic, treating a nil heuristic as "not
// complex" (the conservative single-loop default).
func (r *SupervisorFirstRouter) supervise(goal string) bool {
	return r.ShouldSupervise != nil && r.ShouldSupervise(goal)
}

// logRoute records the metadata-only session_route audit: the chosen route and the
// reason LENGTH (never the reason body — I5/I7), plus whether the pure-function
// fallback was taken. nil-safe.
func (r *SupervisorFirstRouter) logRoute(route Route, reasonLen int, fallback bool) {
	r.Log.Append(eventlog.Event{
		Task: r.ID,
		Kind: "session_route",
		Detail: map[string]any{
			"route":      route.String(),
			"reason_len": reasonLen,
			"fallback":   fallback,
		},
	})
}

// parseRoute extracts the first JSON object from the classifier text and maps its
// "route" field to a Route. It tolerates a chatty wrapper around the object (same
// brace-extraction discipline as summarize.parse / route.Review). ok is false when
// no object parses or the route string is unrecognized — the caller then falls
// back to the heuristic.
func parseRoute(s string) (route Route, reason string, ok bool) {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return RouteContinue, "", false
	}
	var v struct {
		Route  string `json:"route"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &v); err != nil {
		return RouteContinue, "", false
	}
	switch strings.ToLower(strings.TrimSpace(v.Route)) {
	case "chat":
		return RouteChat, v.Reason, true
	case "native":
		return RouteNative, v.Reason, true
	case "supervise", "supervisor", "feature":
		return RouteSupervise, v.Reason, true
	case "project":
		return RouteProject, v.Reason, true
	default:
		// A parseable object with an unknown route string is treated as a parse
		// failure so the caller takes the heuristic fallback.
		return RouteContinue, "", false
	}
}

// referencesGoal is the local, no-model RouteContinue rule: a non-empty active goal
// whose distinctive words the follow-up message names (or an explicit "continue"
// verb) means the message continues the current work. Conservative by design — when
// in doubt it returns false and lets the classifier route a fresh drive, since a
// mis-continue would re-enter the wrong driver. Empty goal ⇒ never a continue.
func referencesGoal(text, goal string) bool {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return false
	}
	lower := strings.ToLower(text)

	// Explicit continuation verbs always continue the active work.
	for _, v := range []string{"continue", "keep going", "carry on", "go on", "finish it", "resume"} {
		if strings.Contains(lower, v) {
			return true
		}
	}

	// Otherwise: does the message name a distinctive (long) word from the goal? A
	// shared significant token is a strong signal the follow-up is about the same
	// work. Short/common words are ignored to avoid spurious continues.
	goalWords := significantWords(goal)
	if len(goalWords) == 0 {
		return false
	}
	msgWords := wordSet(lower)
	for w := range goalWords {
		if msgWords[w] {
			return true
		}
	}
	return false
}

// significantWords returns the set of lowercased goal words long enough to be
// distinctive (≥5 runes), so generic words ("add", "the", "fix") don't trigger a
// false continue.
func significantWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), isWordBreak) {
		if len([]rune(w)) >= 5 {
			out[w] = true
		}
	}
	return out
}

// wordSet returns the set of words in s (already lowercased by the caller).
func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(s, isWordBreak) {
		out[w] = true
	}
	return out
}

// isWordBreak splits on anything that is not a letter or digit.
func isWordBreak(r rune) bool {
	return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
}

// firstTextBlock returns the first non-empty text block — the same defensive read
// route.firstText / summarize.firstText use, kept package-local so the router has
// no cross-package dependency on an unexported helper.
func firstTextBlock(blocks []model.Block) string {
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			return b.Text
		}
	}
	return ""
}
