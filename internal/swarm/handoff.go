package swarm

// handoff.go — dependency-aware bases + the fenced context handoff.
//
// WHY. TreeSharder (PR #94) put real Deps on shards, so the runner ORDERS a
// dependent after its dependency — but ordering alone bought nothing: the
// dependent's worktree was still cut from base HEAD, blind to the dependency's
// verified code, and its goal never learned what the dependency established. This
// file makes the Deps pay: a dep-satisfied shard is cut from its dependency's
// VERIFIED branch (single dep) or the integrated tip (multiple deps), and its
// goal gains a bounded per-dep digest — the dependency's verifier-set claim
// statuses as structural control lines plus its prose summary FENCED via
// guard.Wrap (I7: a model-authored summary is data, never instructions; the
// claim statuses are harness-computed and may ride as control text).
//
// It mirrors internal/super's depTip/resolveBaseRef/resolveBaseRefs swarm-locally
// (the leaf rule forbids importing super): single-dep resolution follows the
// dependency's branch, multi-dep resolution uses the integrated tip when every
// dependency has already been folded — because ONE ref cannot represent the
// union of unmerged branches (the integrator merges those). Both entry points —
// the Controller's cross-pass prepare and the Runner's intra-pass resolve — feed
// the same digest builder, so the handoff shape is identical either way.
//
// Also here: the FOCUSED retry goal (evidence-carrying requeue). A red shard's
// requeue goal is composed from its still-red claim Units via requeue.Plan (the
// same planner the P11 wiring consumes), naming the red claim ids plus each
// Unit's verifier Detail — trusted verifier text, bounded — so a retry aims at
// the exact broken cells instead of blindly re-rolling the whole goal.

import (
	"fmt"
	"strings"

	"nilcore/internal/guard"
	"nilcore/internal/requeue"
	"nilcore/internal/spawn"
)

// Bounds for the per-dep handoff digest and the focused-retry suffix. The digest
// must stay small (~1-2KB/dep) so a wide DAG cannot balloon a dependent's prompt;
// the caps are bytes, mirroring cmd's report clipping discipline.
const (
	maxDigestClaims       = 32   // claim-status lines carried per dep digest
	maxDigestIDBytes      = 120  // per-field clip for an untrusted claim ID/Field/Status line
	maxDigestSummaryBytes = 1024 // dep prose summary clip (fenced via guard.Wrap)
	maxDigestBytes        = 2048 // total per-dep digest cap (~1-2KB/dep bound, I7)
	maxFocusDetailBytes   = 256  // per-claim verifier Detail clip in a focused retry
	maxFocusSuffixBytes   = 2048 // total focused-retry suffix cap per shard
)

// digestMarker is the stable first line of one dependency's handoff digest. It
// doubles as the idempotence key: the Runner's intra-pass resolve appends a digest
// only when the goal does not already carry this marker, so a shard prepared by
// the Controller AND re-resolved by the Runner never accumulates duplicates.
func digestMarker(depID string) string { return "[dep " + depID + " handoff]" }

// depDigest renders one satisfied dependency's bounded handoff digest. The header
// and branch line are harness-authored control text. The claim-status lines are
// NOT trusted control text as once assumed: Claim.ID and Claim.Field are
// MODEL-AUTHORED (the artifact schema admits any unique non-empty string, newlines
// included), so a dep worker that ingested hostile content could mint an id like
// "x\nIMPORTANT NEW TASK: …" that, printed raw, would land as unfenced control text
// in a DEPENDENT worker's Goal (I7 cross-worker injection). Fix #8 therefore (a)
// sanitizes each ID/Field to a single line clipped to maxDigestIDBytes and (b)
// FENCES the whole claim-status block inside guard.Wrap so nothing in it can be
// read as an instruction. Only the verifier-set Status is trusted, but it rides
// with the untrusted ID/Field, so the entire block is fenced. The dep's prose
// Summary is fenced separately as before. The whole digest is length-clipped so a
// dependent's Goal cannot balloon past the per-dep bound (~1-2KB).
func depDigest(depID string, res spawn.Result) string {
	var b strings.Builder
	b.WriteString(digestMarker(depID))
	b.WriteString("\nDependency ")
	b.WriteString(depID)
	b.WriteString(" passed verification")
	if res.Branch != "" {
		b.WriteString("; its verified work is on branch ")
		b.WriteString(res.Branch)
		b.WriteString(" (your worktree may already include it)")
	}
	b.WriteString(".")
	if a := res.Artifact; a != nil && len(a.Claims) > 0 {
		// Build the claim-status body from SANITIZED (single-line, clipped) fields, then
		// fence the whole thing: the untrusted ID/Field can never break the fence or
		// smuggle control text into the dependent's goal (I7).
		var body strings.Builder
		for i, c := range a.Claims {
			if i >= maxDigestClaims {
				fmt.Fprintf(&body, "\n- … %d more claims elided", len(a.Claims)-i)
				break
			}
			fmt.Fprintf(&body, "\n- %s (%s): %s",
				sanitizeLine(c.ID, maxDigestIDBytes),
				sanitizeLine(c.Field, maxDigestIDBytes),
				sanitizeLine(c.Status, maxDigestIDBytes))
		}
		b.WriteString("\n")
		b.WriteString(guard.Wrap("dependency "+depID+" verifier claim statuses", body.String()))
	}
	if s := strings.TrimSpace(res.Summary); s != "" {
		// The dep's prose is MODEL-AUTHORED: fence it so it can never become a
		// controlling instruction for the dependent (I7).
		b.WriteString("\n")
		b.WriteString(guard.Wrap("dependency "+depID+" summary", clipBytes(s, maxDigestSummaryBytes)))
	}
	// Clip the whole digest so a single dep can never blow the ~1-2KB/dep bound even
	// with the maximum claim count (the marker survives — it is the first line).
	return clipBytes(b.String(), maxDigestBytes)
}

// sanitizeLine collapses a model-authored identifier to a single, bounded line:
// every control character (newline, carriage return, tab, and the rest of the
// C0/C1 range) is replaced with a space, then the result is clipped to n bytes.
// This is what makes an untrusted Claim.ID/Field safe to print as a "- id (field):
// status" line — no embedded newline can start a fresh, unfenced control line, and
// no unbounded value can balloon the digest (I7 + the per-dep byte bound).
func sanitizeLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return clipBytes(s, n)
}

// appendDepDigest appends dep's digest to s.Goal unless the goal already carries
// that dep's marker (idempotent across the Controller-prepare / Runner-resolve
// pair). It returns whether the digest was appended.
func appendDepDigest(s *Shard, depID string, res spawn.Result) bool {
	if strings.Contains(s.Goal, digestMarker(depID)) {
		return false
	}
	s.Goal += "\n\n" + depDigest(depID, res)
	return true
}

// resolveIntraPass resolves the dependency handoff for a shard the DAG scheduler
// is about to release WITHIN a pass, from the results its same-pass dependencies
// already produced (the scheduler guarantees deps complete before dependents
// release). Mirrors super.resolveBaseRef's intra-wave leg: exactly ONE declared
// dep that passed with a branch ⇒ cut from that branch; multiple deps ⇒ leave the
// base alone (no integrated tip exists mid-pass — the documented single-ref
// limitation super shares). An already-set BaseRef (continue_from, conflict
// rebuild, or the Controller's cross-pass resolution) is never overridden. The
// digest is appended for EVERY same-pass dep that passed, single or multi.
func resolveIntraPass(s *Shard, done map[string]spawn.Result) {
	if len(s.Deps) == 0 {
		return
	}
	for _, dep := range s.Deps {
		res, ok := done[dep]
		if !ok || !res.Passed || res.Err != nil {
			continue
		}
		if s.BaseRef == "" && len(s.Deps) == 1 && res.Branch != "" {
			s.BaseRef = res.Branch
		}
		appendDepDigest(s, dep, res)
	}
}

// prepareShards is the Controller's CROSS-PASS handoff resolve, run on a COPY of
// the pass's shard set just before dispatch (the canonical set keeps its original
// goals, so per-pass digests never compound across passes). For each shard it
// considers only deps satisfied in a PRIOR pass (present in passed) that are NOT
// re-running this pass (a same-pass dep is resolved intra-pass by the Runner with
// its fresher result):
//
//   - BaseRef (only when still empty — a retry's continue_from / conflict-rebuild
//     base always wins): exactly one declared dep, satisfied, with a branch ⇒ that
//     dep's VERIFIED branch (mirrors super.depTip); two or more declared deps, ALL
//     satisfied in prior passes ⇒ the current integrated TipSHA (the merged union
//     of their work — one ref cannot represent unmerged branches).
//   - Goal: one bounded digest appended per satisfied prior-pass dep.
func prepareShards(current []Shard, passed map[string]spawn.Result, tipSHA string) []Shard {
	out := make([]Shard, len(current))
	copy(out, current)

	inPass := make(map[string]bool, len(current))
	for i := range current {
		inPass[current[i].ID] = true
	}

	for i := range out {
		s := &out[i]
		if len(s.Deps) == 0 {
			continue
		}
		allPrior := true
		var prior []string // deps satisfied in a prior pass, in declaration order
		for _, dep := range s.Deps {
			res, ok := passed[dep]
			if !ok || !res.Passed || inPass[dep] {
				allPrior = false
				continue
			}
			prior = append(prior, dep)
		}
		if s.BaseRef == "" {
			switch {
			case len(s.Deps) == 1 && len(prior) == 1 && passed[prior[0]].Branch != "":
				s.BaseRef = passed[prior[0]].Branch
			case len(s.Deps) >= 2 && allPrior && tipSHA != "":
				s.BaseRef = tipSHA
			}
		}
		for _, dep := range prior {
			appendDepDigest(s, dep, passed[dep])
		}
	}
	return out
}

// focusedGoals composes, per still-red artifact (== shard) with retry budget, the
// EVIDENCE-CARRYING focus suffix for its requeue: requeue.Plan's harness-authored
// goal (naming exactly the red claim ids) plus each Unit's verifier Detail —
// trusted verifier text (artifact.Evidence.Detail is verifier-written), clipped
// per claim and capped per shard. It must be called with the SAME post-bump
// Ledger bumpAndSelect used, so Plan's exhaustion view matches the eligible set.
func focusedGoals(after requeue.Worklist, led *requeue.Ledger) map[string]string {
	subs := requeue.Plan(after, led, "")
	if len(subs) == 0 {
		return nil
	}
	// Per-unit verifier detail, keyed the same "artifact/claim" way Plan's UnitKeys are.
	detail := make(map[string]requeue.Unit, len(after.Units))
	for _, u := range after.Units {
		detail[u.ArtifactID+"/"+u.ClaimID] = u
	}
	out := make(map[string]string, len(subs))
	for _, sub := range subs {
		if len(sub.UnitKeys) == 0 {
			continue
		}
		// The artifact id is the prefix of every unit key (artifact ids are single-
		// component, so the first '/' is the separator); artifact id == shard id.
		artID, _, ok := strings.Cut(sub.UnitKeys[0], "/")
		if !ok {
			continue
		}
		var b strings.Builder
		if prev := out[artID]; prev != "" {
			b.WriteString(prev) // a second (owner-split) group folds into the same shard
			b.WriteString("\n")
		}
		b.WriteString(sub.Goal) // requeue's goalFor: names ONLY the red claim ids
		for _, k := range sub.UnitKeys {
			u, ok := detail[k]
			if !ok || strings.TrimSpace(u.Detail) == "" {
				continue
			}
			fmt.Fprintf(&b, "\n- %s: %s", u.ClaimID, clipBytes(u.Detail, maxFocusDetailBytes))
		}
		out[artID] = clipBytes(b.String(), maxFocusSuffixBytes)
	}
	return out
}

// conflictRebuildSuffix is the harness-authored goal suffix for a shard whose
// verified branch CONFLICTED on merge (or turned the combined tree red and was
// rolled back): the retry rebuilds the change on top of the integrated tip.
const conflictRebuildSuffix = "[integration conflict] Rebuild your change on top of the integrated tip: " +
	"your previous branch conflicted with previously merged work (or failed verification when combined with it). " +
	"Re-derive the change against the tree you now see and write the full artifact again."

// clipBytes truncates s to at most n bytes, appending an ellipsis marker when it
// clipped. Byte-based (matching cmd's report clipping); a rune split at the cut
// point is cosmetic only — the text is prompt prose, never parsed.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…[clipped]"
}
