// Package integrate merges the parallel branches produced by subagents into one
// verifier-green tree, one branch at a time, in topological order.
//
// The discipline is the convergence invariant (CLAUDE.md I2): no unverified
// state is ever the integration tip. Each branch is merged with --no-ff
// --no-commit; a conflict is rolled back cleanly (git merge --abort) and the
// branch is preserved for a re-plan; a clean merge is committed and then the
// project verifier is re-run on the merged tree — pass keeps it, fail rolls the
// tree back to the exact pre-merge SHA (git reset --hard). The maximal green
// prefix survives; a red combination is dropped without poisoning the tip.
//
// The Integrator NEVER pushes or lands to the base branch — it only returns the
// green integration worktree. Promotion onto the real branch is the project
// loop's single gated, irreversible step; everything here happens in a throwaway
// worktree and is reversible by construction, so no integration step is ever
// routed through policy.Gate (this is the policy.Classify substring trap the
// design calls out: "merge"/"reset --hard" are reversible here, never gated).
//
// Host-side git runs over a tree a model authored, so every invocation goes
// through the shared hardening clamp (tools.HardenArgs + tools.HardenedEnv,
// invariant I4): a repo-authored .git/hooks/post-merge can never execute on the
// host. Any sibling file read for context is fenced with guard.Wrap (I7).
package integrate

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"nilcore/internal/eventlog"
	"nilcore/internal/tools"
	"nilcore/internal/verify"
	"nilcore/internal/worktree"
)

// Verifier is the done-authority seam over the merged tree. It is the same shape
// as verify.Verifier (and agent.Env.Verifier), declared locally so this package
// does not import internal/agent: the project loop wires agent.Env into the
// orchestrator, agent → ... → project → integrate, so an integrate → agent edge
// would close an import cycle. Defining the minimal seam here keeps integrate a
// leaf the supervisor and project loop can both depend on (CLAUDE.md §4: leaf
// packages must not import the orchestrator). verify.Report is reused unchanged.
type Verifier interface {
	Check(ctx context.Context) (verify.Report, error)
}

// Env is the per-tree execution environment the Integrator needs: just the
// verifier pointed at the merged worktree. It mirrors agent.Env's verifier field
// without importing agent (see Verifier above). The orchestrator's NewEnv factory
// (dir → backend+verifier) is adapted to this narrower shape at the wiring site.
type Env struct {
	Verifier Verifier
}

// MergeItem is one branch to fold in, in the order it is presented. The caller
// (the DAG scheduler / supervisor) supplies branches already in topological
// order, so integration order == topological order and dependents are merged
// after the dependencies they were coded on top of.
type MergeItem struct {
	ID     string // subagent / subtask id, for logging and result correlation
	Branch string // the branch to merge (e.g. "task/super.t1"); spawn.Result.Branch
}

// MergeResult is the outcome of attempting to fold one MergeItem in. Exactly one
// of {Merged&&Verified, Conflict, !Verified} describes the terminal disposition:
//   - Merged && Verified: kept on the tip; SHA is the merge commit.
//   - Conflict:           clean rollback (merge --abort); branch preserved; escalate.
//   - Merged && !Verified: verify failed; tree reset to PreSHA; escalate.
//
// Escalate is set whenever the supervisor must re-plan around this branch (a
// conflict or a verify failure). Err carries any unexpected git/verify error
// (an aborted run), distinct from a normal conflict/red-tree disposition.
type MergeResult struct {
	ID       string
	Branch   string
	PreSHA   string // integration tip before this merge was attempted
	SHA      string // merge commit sha when Merged && Verified; else == PreSHA after rollback
	Merged   bool   // a clean (conflict-free) merge was committed
	Verified bool   // the project verifier passed on the merged tree
	Conflict bool   // the merge had conflicts and was aborted
	Escalate bool   // the supervisor should re-plan around this branch
	Err      error  // unexpected error (not a normal conflict/red outcome)
}

// Integrator folds subagent branches into one integration worktree, re-verifying
// after every merge and rolling back any branch that conflicts or turns the tree
// red. It holds no credentials and never lands to the base branch.
type Integrator struct {
	// BaseRepo is the git repo the throwaway integration worktree is cut from.
	BaseRepo string
	// BaseRef is the committish the throwaway integration worktree starts from.
	// Empty ⇒ "HEAD" (the default — byte-identical to before). A durable-resume
	// run sets it to the preserved integration-tip SHA so a re-integration after a
	// restart folds the remaining branches ON TOP of the already-merged work,
	// instead of rebuilding from base HEAD (which would orphan the verified tip).
	// It must resolve in BaseRepo; an unresolvable ref is a clean setup error.
	BaseRef string
	// NewEnv builds the per-tree environment (the verifier) for a directory —
	// the same factory shape the orchestrator uses, adapted to the local Env.
	NewEnv func(dir string) Env
	// Log is the shared append-only audit trail; integration_* events carry
	// metadata only (ids, shas, pass/fail), never file contents (I5).
	Log *eventlog.Log
}

// Integrate folds each MergeItem into a fresh integration worktree in the order
// given (the caller's topological order), re-verifying after each merge. It
// returns the integration worktree (the green tip — the caller owns Cleanup), a
// per-item MergeResult slice, and an error only for a setup failure that
// prevented integration from starting. Per-branch conflicts and red trees are
// reported in the results (with Escalate set), not as the returned error, so a
// partial integration still hands back the maximal green prefix.
//
// The returned worktree is non-nil whenever setup succeeded, even if some (or
// all) branches were rolled back: its tip is always a verified state — either the
// base HEAD (nothing merged cleanly) or the last branch that merged green.
func (it *Integrator) Integrate(ctx context.Context, order []MergeItem) (*worktree.Worktree, []MergeResult, error) {
	if it.NewEnv == nil {
		return nil, nil, fmt.Errorf("integrate: NewEnv is required")
	}
	if it.BaseRepo == "" {
		return nil, nil, fmt.Errorf("integrate: BaseRepo is required")
	}

	// One throwaway worktree off the integration base. The caller owns Cleanup so it
	// can inspect / promote the green tip first; on a setup failure we clean up.
	// BaseRef defaults to HEAD; a durable-resume run pins it to the preserved tip so
	// the remaining branches fold on top of the already-merged work (no work lost).
	start := it.BaseRef
	if start == "" {
		start = "HEAD"
	}
	branch := "integrate/" + uniqueSuffix()
	leaf := strings.ReplaceAll(branch, "/", "-")
	wt, err := worktree.CreateFrom(ctx, it.BaseRepo, branch, leaf, start)
	if err != nil {
		return nil, nil, fmt.Errorf("integrate: create integration worktree: %w", err)
	}

	it.Log.Append(eventlog.Event{Kind: "integration_start",
		Detail: map[string]any{"branch": branch, "items": len(order)}})

	dir := wt.Path()
	env := it.NewEnv(dir)
	results := make([]MergeResult, 0, len(order))

	for _, item := range order {
		// The tip before this merge is the rollback target. Each accepted merge
		// advances it; a rejected merge restores it, so it is always a verified
		// state (the convergence invariant).
		preSHA, err := wt.Head(ctx)
		if err != nil {
			// Cannot read the tip → cannot guarantee a safe rollback target. Stop
			// integrating; hand back what merged green so far via the worktree.
			results = append(results, MergeResult{ID: item.ID, Branch: item.Branch,
				Escalate: true, Err: fmt.Errorf("read integration tip: %w", err)})
			break
		}
		results = append(results, it.mergeOne(ctx, dir, env, item, preSHA))
	}

	return wt, results, nil
}

// mergeOne attempts a single --no-ff merge of item.Branch into the integration
// worktree at dir, whose current tip is preSHA. It never returns an error from
// the function signature — every disposition (clean+green, conflict, red) is a
// MergeResult, because at the integration level a branch that does not combine
// is a planning result, not a program fault (mirrors verify: a failing check is
// a result, not a Go error).
func (it *Integrator) mergeOne(ctx context.Context, dir string, env Env, item MergeItem, preSHA string) MergeResult {
	res := MergeResult{ID: item.ID, Branch: item.Branch, PreSHA: preSHA, SHA: preSHA}

	// --no-ff keeps a distinct merge commit per branch so a later rollback is one
	// reset; --no-commit lets us re-verify before the merge is recorded. (We do
	// commit clean merges below; verify-then-reset gives per-branch granularity.) The
	// inline -c identity is set on the MERGE too (not just the commit below): git
	// validates the committer up-front even with --no-commit, so without it a host with
	// no git identity (HardenedEnv blanks the global config) fails the merge with
	// "Committer identity unknown". This makes the integrator self-sufficient, matching
	// worktree.Merge — it never depends on ambient GIT_*/config identity.
	out, mergeErr := git(ctx, dir,
		"-c", "user.email=agent@nilcore.local", "-c", "user.name=nilcore",
		"merge", "--no-ff", "--no-commit", item.Branch)
	if mergeErr != nil {
		// A merge that does not apply cleanly leaves the tree mid-merge. Abort to
		// restore the pre-merge tip exactly, then escalate — the conflicting branch
		// is untouched in the base repo and preserved for a re-plan / retry.
		if _, aerr := git(ctx, dir, "merge", "--abort"); aerr != nil {
			// Could not even abort: surface the original conflict plus the abort
			// failure so the audit trail shows the tree may be dirty.
			res.Conflict, res.Escalate = true, true
			res.Err = fmt.Errorf("merge %s conflicted and abort failed: %v (%s)", item.Branch, aerr, strings.TrimSpace(out))
			it.logConflict(item, preSHA, true)
			return res
		}
		res.Conflict, res.Escalate = true, true
		it.logConflict(item, preSHA, false)
		return res
	}

	// Clean merge: record it so verify runs against a real commit, then re-run the
	// project verifier on the merged tree. The verdict — not the merge succeeding —
	// is what governs whether this branch stays on the tip (I2). The commit pins an
	// inert committer identity (matching worktree.Commit) so it never depends on
	// host/global git config, and runs under the same I4 hardening clamp.
	if cout, cerr := git(ctx, dir,
		"-c", "user.email=agent@nilcore.local", "-c", "user.name=nilcore",
		"commit", "--no-edit", "-m", "integrate "+item.Branch); cerr != nil {
		// Failed to record the merge: roll back to the pre-merge tip and escalate.
		_, _ = git(ctx, dir, "reset", "--hard", preSHA)
		res.Escalate = true
		res.Err = fmt.Errorf("commit merge %s: %w (%s)", item.Branch, cerr, strings.TrimSpace(cout))
		return res
	}
	sha, herr := wtHead(ctx, dir)
	if herr != nil {
		_, _ = git(ctx, dir, "reset", "--hard", preSHA)
		res.Escalate = true
		res.Err = fmt.Errorf("read merge sha %s: %w", item.Branch, herr)
		return res
	}
	res.Merged = true

	rep, verr := env.Verifier.Check(ctx)
	if verr != nil {
		// The verifier itself errored (e.g. sandbox failure) — treat as not-green:
		// roll back so the tip stays verified, and escalate with the error.
		_, _ = git(ctx, dir, "reset", "--hard", preSHA)
		res.SHA = preSHA
		res.Escalate = true
		res.Err = fmt.Errorf("verify merged tree (%s): %w", item.Branch, verr)
		it.logRollback(item, preSHA, sha)
		return res
	}

	if !rep.Passed {
		// Green-alone but red-combined: drop this branch off the tip. reset --hard
		// to the pre-merge SHA restores the last verified state exactly. This is a
		// reversible, ungated step (the integrator never gates a throwaway reset).
		if _, rerr := git(ctx, dir, "reset", "--hard", preSHA); rerr != nil {
			res.Err = fmt.Errorf("rollback %s to %s: %w", item.Branch, preSHA, rerr)
		}
		res.SHA = preSHA
		res.Verified = false
		res.Escalate = true
		it.logRollback(item, preSHA, sha)
		return res
	}

	// Kept: this merge is the new verified tip.
	res.SHA = sha
	res.Verified = true
	it.Log.Append(eventlog.Event{Task: item.ID, Kind: "integration_verify",
		Detail: map[string]any{"branch": item.Branch, "passed": true, "sha": sha}})
	it.Log.Append(eventlog.Event{Task: item.ID, Kind: "integration_merge",
		Detail: map[string]any{"branch": item.Branch, "pre_sha": preSHA, "sha": sha}})
	return res
}

func (it *Integrator) logConflict(item MergeItem, preSHA string, abortFailed bool) {
	it.Log.Append(eventlog.Event{Task: item.ID, Kind: "integration_conflict",
		Detail: map[string]any{"branch": item.Branch, "pre_sha": preSHA,
			"abort_failed": abortFailed, "escalate": true}})
}

func (it *Integrator) logRollback(item MergeItem, preSHA, mergeSHA string) {
	it.Log.Append(eventlog.Event{Task: item.ID, Kind: "integration_rollback",
		Detail: map[string]any{"branch": item.Branch, "pre_sha": preSHA,
			"reverted_sha": mergeSHA, "escalate": true}})
}

// git runs a hardening-clamped git subcommand in dir and returns its combined
// output. The clamp is identical to internal/worktree.git and the `git` tool
// (tools.HardenArgs `-c` flags + tools.HardenedEnv), so a repo-authored hook,
// fsmonitor binary, or external config can never execute on the host during a
// host-side merge/abort/reset over model-authored content (I4). Both halves of
// the clamp must always travel together.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	full := append(tools.HardenArgs(), args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = tools.HardenedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// wtHead reads the current HEAD sha of the integration worktree directly (rather
// than holding a *worktree.Worktree, which we only have as the return value) so
// it runs under the same hardening clamp as every other host-side git here.
func wtHead(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// integSeq makes each integration worktree branch name unique within a process,
// so concurrent or repeated Integrate calls never collide on a branch name.
var integSeq atomic.Uint64

// uniqueSuffix is a short, collision-free suffix for the throwaway integration
// branch/leaf. It is purely internal naming — never a security boundary — so a
// monotonic counter plus a timestamp is sufficient (no crypto randomness needed,
// keeping the package stdlib-arithmetic only, I6).
func uniqueSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatUint(integSeq.Add(1), 36)
}
