package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"nilcore/internal/kernel"
	"nilcore/internal/worktree"
)

// decompose.go — the recursive `decompose` preset (Phase 16, Pillar 8 — UOK V2 K2-1,
// docs/ROADMAP-KERNEL-V2.md §5). It is the first PRODUCTION consumer of the kernel's
// native recursive engine (kernel.Recursive): a goal is PLANNED into independent
// sub-goals, each runs as a full single-task run in its own worktree (KeepBranch so its
// verified branch survives), and the verified child branches are INTEGRATED into one tip
// — merging each, re-verifying after every merge, and DROPPING any child that conflicts
// or turns the tree red (I2: the integrated tip is the verifier's verdict, never the
// children's). It is OPT-IN (default-off) + the integrator's logic is hermetically
// tested; the real-backend end-to-end is the field-validation step (mirroring how the
// kernel itself shipped opt-in + proven before flipping on).

// childResult is one planned sub-goal's verified output: the worktree branch its run
// produced. A child is mergeable only when it verified AND kept a branch.
type childResult struct {
	subGoal  string
	branch   string
	verified bool
}

// numberedItem matches a leading list marker ("1.", "2)", "-", "*") so a planned goal
// written as a list splits on its items.
var numberedItem = regexp.MustCompile(`^\s*(?:\d+[.)]|[-*])\s+`)

// decomposePlan splits a goal into independent sub-goals — the deterministic default
// Plan (a model-backed splitter is the future seam, exactly like router.Oracle). It
// recognizes, in order: explicit newlines / list items; then a top-level " and " / ";"
// / ", and " join. A goal with no separable parts yields a single sub-goal (the whole
// goal), so decompose degenerates to one ordinary run rather than failing. The goal is
// inert DATA (I7) — this only segments text, never executes it.
func decomposePlan(goal string) []string {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil
	}
	// 1) Multi-line / list form: one sub-goal per non-empty line, stripping list markers.
	if strings.Contains(goal, "\n") {
		var subs []string
		for _, ln := range strings.Split(goal, "\n") {
			ln = strings.TrimSpace(numberedItem.ReplaceAllString(ln, ""))
			if ln != "" {
				subs = append(subs, ln)
			}
		}
		if len(subs) > 1 {
			return subs
		}
		if len(subs) == 1 {
			goal = subs[0]
		}
	}
	// 2) Single-line conjunction: split on ';' or a top-level ' and '. Kept simple +
	// conservative — a goal that does not clearly fork stays one sub-goal.
	parts := splitConjuncts(goal)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(strings.TrimRight(p, ".")); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{goal}
	}
	return out
}

// splitConjuncts splits a one-line goal on ';' and the word ' and ' (and ', and ').
func splitConjuncts(s string) []string {
	s = strings.ReplaceAll(s, ", and ", ";")
	s = strings.ReplaceAll(s, " and ", ";")
	return strings.Split(s, ";")
}

// verifyFunc runs the project verifier on a worktree path and reports whether it passed.
// It is injected so integrateBranches' merge-and-re-verify logic is hermetically testable
// with a fake verifier (the real one is the sandboxed project verifier the children use).
type verifyFunc func(ctx context.Context, dir string) (bool, error)

// integrateResult is the integrator's report: the integration branch, whether its final
// tip verifies (I2), and how many children were merged in (the rest were dropped).
type integrateResult struct {
	branch   string
	verified bool
	merged   int
	dropped  int
}

// integrateBranches merges each verified child branch into a FRESH integration worktree
// cut from baseRepo HEAD, re-verifying after EVERY merge and DROPPING any child that
// conflicts (Merge auto-aborts, restoring the tip) or turns the tree red (reset to the
// last green tip). The final tip is verified iff the project verifier passes on it — the
// merged children never decide "done" (I2). The caller owns the returned worktree's
// lifecycle (keep its branch on success, Cleanup otherwise).
func integrateBranches(ctx context.Context, baseRepo, integLeaf string, children []childResult, verify verifyFunc) (*worktree.Worktree, integrateResult, error) {
	wt, err := worktree.CreateFrom(ctx, baseRepo, "task/"+integLeaf, integLeaf, "HEAD")
	if err != nil {
		return nil, integrateResult{}, fmt.Errorf("decompose: integration worktree: %w", err)
	}
	res := integrateResult{branch: wt.Branch()}
	for _, c := range children {
		if !c.verified || c.branch == "" {
			res.dropped++
			continue
		}
		prev, herr := wt.Head(ctx)
		if herr != nil {
			_ = wt.Cleanup()
			return nil, integrateResult{}, fmt.Errorf("decompose: read tip: %w", herr)
		}
		conflict, merr := wt.Merge(ctx, c.branch, "decompose: integrate "+c.subGoal)
		if merr != nil { // a failed abort left the tree dirty — a real fault
			_ = wt.Cleanup()
			return nil, integrateResult{}, fmt.Errorf("decompose: merge %q: %w", c.subGoal, merr)
		}
		if conflict {
			res.dropped++ // Merge restored the pre-merge tip; the child is dropped
			continue
		}
		ok, verr := verify(ctx, wt.Path())
		if verr != nil || !ok {
			// The merge applied but turned the tree red — undo it so the next child
			// merges onto the last green tip (I2: a red tip is never kept).
			if rerr := wt.Reset(ctx, prev); rerr != nil {
				_ = wt.Cleanup()
				return nil, integrateResult{}, fmt.Errorf("decompose: undo red merge %q: %w", c.subGoal, rerr)
			}
			res.dropped++
			continue
		}
		res.merged++
	}
	// I2: the integrated tip is "done" only if the verifier passes on the final tree —
	// re-checked here even after the per-merge checks (the base itself may be red, and a
	// zero-merge decompose must not claim verified on an unchecked tip).
	final, ferr := verify(ctx, wt.Path())
	if ferr != nil {
		_ = wt.Cleanup()
		return nil, integrateResult{}, fmt.Errorf("decompose: final verify: %w", ferr)
	}
	res.verified = final
	return wt, res, nil
}

// childRunner runs ONE sub-goal as a full single-task run that KEEPS its verified branch,
// returning the branch name + verifier verdict. Injected so the cmd entry supplies the
// real KeepBranch orchestrator and the envelope flow stays hermetically testable.
type childRunner func(ctx context.Context, subGoal, taskID string) (branch string, verified bool, err error)

// decomposeState carries the integration worktree out of the (pure-data) kernel Outcome
// so the caller can keep its branch on success or Cleanup otherwise.
type decomposeState struct {
	wt  *worktree.Worktree
	res integrateResult
}

// decomposeEnvelope assembles the recursive `decompose` preset over kernel.Recursive
// (UOK V2 K2-1): Plan = decomposePlan, each child runs via runChild (a KeepBranch
// single-task run), Integrate = integrateBranches (merge the verified child branches into
// one re-verified tip — I2). It is bounded by maxChildren (fail-closed) and depth 1, and
// obs records the recursion tree to the log. The returned decomposeState exposes the
// integration worktree for the caller's keep/clean decision after kernel.Run returns.
func decomposeEnvelope(rootID, baseRepo string, runChild childRunner, verify verifyFunc, maxChildren int, obs kernel.Observer) (kernel.Envelope, *decomposeState) {
	st := &decomposeState{}
	env := kernel.Envelope{
		Name:        "decompose",
		Granularity: kernel.AlwaysDecompose,
		MaxDepth:    1, // one level: a goal → its sub-goals (each a flat run)
		MaxChildren: maxChildren,
		Observer:    obs,
		// A child sub-goal runs as a full single-task run that keeps its verified branch.
		Flat: func(ctx context.Context, n kernel.Node) (kernel.Outcome, error) {
			branch, verified, err := runChild(ctx, n.Goal, n.ID)
			if err != nil {
				return kernel.Outcome{}, err
			}
			return kernel.Outcome{Summary: n.Goal, Branch: branch, Verified: verified}, nil
		},
	}
	plan := func(_ context.Context, n kernel.Node) ([]kernel.Node, error) {
		subs := decomposePlan(n.Goal)
		nodes := make([]kernel.Node, len(subs))
		for i, s := range subs {
			nodes[i] = kernel.Node{ID: fmt.Sprintf("%s-%d", rootID, i+1), Goal: s}
		}
		return nodes, nil
	}
	integrate := func(ctx context.Context, n kernel.Node, outs []kernel.Outcome) (kernel.Outcome, error) {
		children := make([]childResult, len(outs))
		for i, o := range outs {
			children[i] = childResult{subGoal: o.Summary, branch: o.Branch, verified: o.Verified}
		}
		wt, res, err := integrateBranches(ctx, baseRepo, n.ID+"-integ", children, verify)
		if err != nil {
			return kernel.Outcome{}, err
		}
		st.wt, st.res = wt, res
		return kernel.Outcome{
			Summary:  fmt.Sprintf("decomposed into %d sub-goals; merged %d, dropped %d", len(outs), res.merged, res.dropped),
			Branch:   res.branch,
			Verified: res.verified,
		}, nil
	}
	env.Decompose = kernel.Recursive(&env, plan, integrate)
	return env, st
}
