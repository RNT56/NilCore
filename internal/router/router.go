// Package router is the preset-routing layer that completes the unified orchestration
// kernel (Phase 16, Pillar 8 — UOK V2, docs/ROADMAP-KERNEL-V2.md). The kernel's own
// doc states its purpose as "the conversational router picks an ENVELOPE, not a machine"
// — but until now the envelope was always chosen by the human typing `run` / `build` /
// `swarm`. This package is that missing router: it maps a goal to the cheapest preset
// that fits, so one entry (`nilcore do`) lets the agent pick how to work.
//
// WHY this is a pure leaf, and why it cannot break an invariant:
//
//   - It imports ONLY the standard library (deps_test.go). It names no kernel/agent/
//     project/swarm package; the cmd layer maps a chosen Preset onto the proven machine.
//   - It only ORDERS the choice of machine. It never decides "done" (I2 — the verifier
//     still judges whatever machine runs) and never approves an irreversible action (I3 —
//     every gate still fires inside the chosen machine). Routing is a bias on WHICH
//     machine attempts the work, never an override of any verdict or gate.
//   - The goal it classifies is inert DATA (I7): keyword matching, never instructions.
//
// DEFAULT path: the deterministic Classify heuristic. A learned/model-backed Oracle is an
// OPTIONAL seam (nil ⇒ Classify), mirroring agent.TrustOracle — the leaf declares the
// shape; a richer router implements it without this package importing anything.
package router

import (
	"context"
	"strings"
)

// Preset names a kernel envelope the router can choose for a goal. The values are exactly
// the existing presets `nilcore do` dispatches to, so a Preset maps 1:1 onto a proven
// machine at the cmd layer.
type Preset string

const (
	// Run is the single-task orchestrator (the kernel's FLAT branch) — the cheapest,
	// safest default: one task driven to a verifier-green tree in a disposable worktree.
	Run Preset = "run"
	// Build is the project loop (a DECOMPOSE machine): drive a whole project/repo to a
	// verifier-green tree across many supervised slices.
	Build Preset = "build"
	// Swarm is the verified swarm (a DECOMPOSE machine): fan a breadth objective out into
	// typed, independently-verified shards, requeued until clean.
	Swarm Preset = "swarm"
)

// Valid reports whether p is one of the known presets.
func (p Preset) Valid() bool {
	switch p {
	case Run, Build, Swarm:
		return true
	default:
		return false
	}
}

// swarmSignals mark a breadth / parallel objective — many independent pieces of work that
// the verified swarm exists to fan out. Checked FIRST because parallel intent is the most
// specialized (and most expensive) shape, so an explicit breadth signal should win.
var swarmSignals = []string{
	"swarm", "in parallel", "fan out", "fan-out", "for each", "for every",
	"each of the", "across all", "across every", "every file", "every package",
	"every module", "every service", "bulk ", "sweep the", "audit the codebase",
}

// buildSignals mark a whole-project / scaffold task — the project loop's shape. Checked
// AFTER swarm so "scaffold N services in parallel" routes to the swarm, not build.
var buildSignals = []string{
	"build a", "build an", "build the project", "create a project", "create a new project",
	"scaffold", "new project", "new service", "new app", "from scratch", "greenfield",
	"whole project", "entire project", "bootstrap a", "set up a project", "spin up a",
}

// Classify is the deterministic default router: a keyword bucket over the goal text that
// picks the cheapest preset that fits. The order (swarm → build → run) biases toward the
// cheapest, safest machine when no specialized signal is present — most goals are single
// tasks, so Run is the fallthrough. The match is over inert lowercased data (I7).
func Classify(goal string) Preset {
	g := strings.ToLower(goal)
	if containsAny(g, swarmSignals) {
		return Swarm
	}
	if containsAny(g, buildSignals) {
		return Build
	}
	return Run
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// Oracle is the optional seam a learned/model-backed router implements to override the
// heuristic — e.g. one informed by the experience/lessons/trust ledgers (the closed-loop
// signals). nil ⇒ Classify. It only PICKS A PRESET; the verifier still judges the run
// (I2) and every irreversible step still gates (I3). Route enforces fail-closed bounds, so
// a degenerate Oracle can never select an unavailable or invalid machine.
type Oracle interface {
	// Route returns the preset the oracle would run goal as, and whether it has a
	// confident opinion. A false ⇒ "no opinion": Route falls back to Classify. The
	// allowed set is the presets the caller is willing to dispatch; an oracle pick
	// outside it is treated as no-opinion (fail-closed).
	Route(ctx context.Context, goal string, allowed []Preset) (Preset, bool)
}

// Route picks the preset for a goal through a possibly-nil Oracle, constrained to the
// allowed set. It is the ONE place the nil / degenerate / out-of-bounds cases are handled,
// so callers stay branch-free. The returned string is the routing PROVENANCE for audit:
// "heuristic" when Classify decided, "oracle" when a wired oracle's confident pick stood.
//
// Fail-closed guarantees:
//   - nil oracle                      ⇒ Classify, provenance "heuristic".
//   - oracle has no opinion           ⇒ Classify, provenance "heuristic".
//   - oracle picks outside `allowed`  ⇒ Classify, provenance "heuristic" (pick ignored).
//   - oracle picks an invalid preset  ⇒ Classify, provenance "heuristic".
//   - Classify's pick not in `allowed`⇒ the first allowed preset (never an empty choice);
//     callers always pass a non-empty allowed set, so a runnable machine is guaranteed.
func Route(ctx context.Context, o Oracle, goal string, allowed []Preset) (Preset, string) {
	pick := Classify(goal)
	provenance := "heuristic"
	if o != nil {
		if p, ok := o.Route(ctx, goal, allowed); ok && p.Valid() && contains(allowed, p) {
			pick, provenance = p, "oracle"
		}
	}
	if !contains(allowed, pick) {
		// The heuristic chose a machine the caller did not allow: fall back to the first
		// allowed preset so Route never returns an unrunnable choice.
		if len(allowed) > 0 {
			return allowed[0], "heuristic"
		}
	}
	return pick, provenance
}

func contains(set []Preset, p Preset) bool {
	for _, s := range set {
		if s == p {
			return true
		}
	}
	return false
}

// All is the default allowed set: every preset `nilcore do` can dispatch to.
func All() []Preset { return []Preset{Run, Build, Swarm} }
