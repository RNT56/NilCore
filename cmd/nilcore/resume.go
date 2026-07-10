// resume.go is the consumption half of multi-agent durable resume (PR-2). The
// substrate (internal/super's SaveState seam + leaf Snapshot, internal/agent's
// RunState/ResumePlan + SuperviseStatus) lets a supervised/project run checkpoint
// its verified integration tip after every merge; this file wires that snapshot at
// the serve boundary and replays it on restart.
//
// The shape, and why each piece exists:
//
//   - A supervise run's store row uses the DISTINCT SuperviseStatus, so the single-
//     native resume pass (resumeInflight → Checkpoint.Resume over running/interrupted)
//     never re-drives a multi-agent run as one native drive (the regression). The
//     dedicated resumeSupervise pass owns it.
//   - The verified tip is pinned with a resume/<taskID> branch the run-end sweep
//     (buildStack's cleanup, which only reclaims task/ rebase/ integrate/ read/) never
//     touches — so the merged work stays reachable across a GRACEFUL restart, where
//     the deferred cleanup would otherwise delete the integrate/ tip.
//   - On restart the preserved tip is RE-VERIFIED before it is trusted (I2 — never
//     build on a stored SHA blind); a tip that no longer passes degrades to a fresh
//     re-run, with the merged work left pinned for manual recovery.
//   - Resume is a SOFT re-plan, not deterministic replay: the supervisor is model-
//     driven, so the rebuilt run is ROOTED at the preserved tip (integrator BaseRef +
//     the worker/code/verify base ref) and the model is told what is already merged so
//     it plans only the remainder. The honest guarantee is "verified merged work
//     preserved and never corrupted" (the integrator's idempotence + the verifier),
//     not "a merged node is literally never re-spawned".
package main

import (
	"context"

	"nilcore/internal/agent"
	"nilcore/internal/budget"
	"nilcore/internal/channel"
	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
	"nilcore/internal/super"
	"nilcore/internal/verify"
	"nilcore/internal/worktree"
)

// superviseTaskID derives the durable store key for a thread's multi-agent run. It is
// distinct from the conversation row (keyed by threadID, status "conversation") and
// the wake row (keyed "wake:<threadID>"), so the three never collide on the same
// primary key in the store.
func superviseTaskID(threadID string) string { return "supervise:" + threadID }

// resumeRef is the durable branch that pins a supervise run's verified integration
// tip. It lives under the resume/ prefix — which the run-end branch sweep never
// reclaims — so the merged commit stays reachable across a graceful restart even after
// its integrate/ branch is swept. Git refs cannot contain ':', so the taskID's
// separator is mapped to '-' (e.g. "supervise:123" → "resume/supervise-123").
func resumeRef(taskID string) string {
	out := make([]byte, 0, len("resume/")+len(taskID))
	out = append(out, "resume/"...)
	for i := 0; i < len(taskID); i++ {
		if taskID[i] == ':' {
			out = append(out, '-')
		} else {
			out = append(out, taskID[i])
		}
	}
	return string(out)
}

// translateSnapshot maps the supervisor's leaf Snapshot to the durable agent.RunState
// the checkpoint persists. The node-state vocabularies are identical, so it is a
// field-for-field copy; doing it HERE (the wiring site) is what keeps internal/super a
// leaf that imports neither the orchestrator nor the store (CLAUDE.md §4, I1).
func translateSnapshot(snap super.Snapshot) agent.RunState {
	nodes := make([]agent.Node, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		nodes = append(nodes, agent.Node{ID: n.ID, DependsOn: n.DependsOn, State: agent.NodeState(n.State)})
	}
	return agent.RunState{TipSHA: snap.TipSHA, Nodes: nodes}
}

// resumeStateFrom builds the supervisor's leaf ResumeState seed from a durable
// snapshot. TipBranch is the durable pin (surfaced as the Outcome branch if a resumed
// run converges with no further integration). It is the inverse of translateSnapshot.
func resumeStateFrom(taskID string, rs agent.RunState) *super.ResumeState {
	nodes := make([]super.ResumeNode, 0, len(rs.Nodes))
	for _, n := range rs.Nodes {
		nodes = append(nodes, super.ResumeNode{ID: n.ID, DependsOn: n.DependsOn, State: string(n.State)})
	}
	return &super.ResumeState{TipSHA: rs.TipSHA, TipBranch: resumeRef(taskID), Nodes: nodes}
}

// finalizeSupervise marks a supervise run's durable row terminal once the drive ran to
// its OWN conclusion (ctx not cancelled — a SIGTERM-interrupted drive is left
// "supervise" so the next boot resumes it). On a verified-done run it also drops the
// resume pin; a not-done terminal leaves the pin so the partial verified work survives
// for manual recovery. nil checkpoint / empty taskID ⇒ no-op.
func finalizeSupervise(ctx context.Context, d serveDeps, taskID, goal string, done bool) {
	if d.checkpoint == nil || taskID == "" || ctx.Err() != nil {
		return
	}
	if err := d.checkpoint.Complete(ctx, taskID, goal, done); err != nil {
		d.log.Append(eventlog.Event{Kind: "supervise_complete_error",
			Detail: map[string]any{"task": taskID, "error": err.Error()}})
		return
	}
	if done {
		// Share gitMu with concurrent drives' worktree/ref ops (a live drive finalizing
		// while another thread spawns workers): dropping the pin is a shared-repo ref
		// mutation. Best-effort — a missed delete just leaves a stale ref the done row
		// already excludes from resume.
		gitMu.Lock()
		worktree.DeleteBranch(context.Background(), d.baseRepo, resumeRef(taskID))
		gitMu.Unlock()
	}
}

// resumeSupervise re-drives every multi-agent run a prior process left in flight (a
// non-terminal SuperviseStatus row), before serve accepts new traffic. For each it
// loads the durable snapshot, RE-VERIFIES the preserved tip (I2), and rebuilds the
// supervisor stack rooted at that tip + seeded with the prior dispositions so the run
// continues from the merged work. A tip that no longer re-verifies degrades to a fresh
// re-run (the pre-resume behavior); the merged work stays pinned for manual recovery.
// Each run is resumed at most once per boot — Complete marks it terminal — so a
// deterministically-faulting run can never poison-loop the restart.
func resumeSupervise(ctx context.Context, d serveDeps, notifyCh channel.Channel) {
	if d.checkpoint == nil {
		return
	}
	tasks, err := d.checkpoint.InFlightSupervise(ctx)
	if err != nil {
		d.log.Append(eventlog.Event{Kind: "resume_supervise_error", Detail: map[string]any{"error": err.Error()}})
		return
	}
	for _, t := range tasks {
		rs, lerr := d.checkpoint.LoadRunState(ctx, t.ID)
		if lerr != nil {
			d.log.Append(eventlog.Event{Kind: "resume_supervise_error",
				Detail: map[string]any{"task": t.ID, "error": lerr.Error()}})
			continue
		}

		// Re-verify the preserved tip before trusting it as the integration base (I2).
		// A green tip ⇒ resume rooted at it + seeded; a tip that no longer passes (or has
		// no SHA) ⇒ a fresh re-run, with the pin left for manual recovery.
		baseRef := ""
		var seed *super.ResumeState
		if rs.TipSHA != "" && verifyResumeTip(ctx, d, rs.TipSHA) {
			baseRef = rs.TipSHA
			seed = resumeStateFrom(t.ID, rs)
		} else if rs.TipSHA != "" {
			d.log.Append(eventlog.Event{Kind: "resume_supervise_tip_unverified",
				Detail: map[string]any{"task": t.ID, "tip": rs.TipSHA}})
		}

		approver := superviseResumeApprover(ctx, notifyCh, t.ID, d.log)
		done, rerr := runResumedSupervise(ctx, d, t.Goal, t.ID, baseRef, seed, approver)
		if rerr != nil {
			// A harness fault on resume is terminal (failed) so it is not retried every
			// boot; the pin is left so the verified tip survives for manual recovery.
			_ = d.checkpoint.Complete(ctx, t.ID, t.Goal, false)
			d.log.Append(eventlog.Event{Kind: "resume_supervise_error",
				Detail: map[string]any{"task": t.ID, "error": rerr.Error()}})
			continue
		}
		finalizeSupervise(ctx, d, t.ID, t.Goal, done)
	}
}

// superviseResumeApprover is the gate approver for a resumed multi-agent run: like the
// native resume path it INFORMS the owner's thread of an irreversible gate then DENIES
// (escalate-on-gate, never auto-approves — I3), or degrades to a silent deny with no
// transport. The supervise task id ("supervise:<threadID>") is not the native
// "<threadID>-<seq>" shape informGateApprover recovers the thread from, so we hand it a
// synthetic "<threadID>-0" id that resolves to the owner thread.
func superviseResumeApprover(ctx context.Context, notifyCh channel.Channel, taskID string, log *eventlog.Log) policy.Approver {
	if notifyCh == nil {
		return denyAllApprover{}
	}
	thread := superviseOwnerThread(taskID)
	return informGateApprover{ch: notifyCh, ctx: ctx, taskID: thread + "-0", log: log}
}

// superviseOwnerThread inverts superviseTaskID to recover the owning channel thread.
func superviseOwnerThread(taskID string) string {
	const p = "supervise:"
	if len(taskID) > len(p) && taskID[:len(p)] == p {
		return taskID[len(p):]
	}
	return taskID
}

// runResumedSupervise assembles a supervise stack via buildStack rooted at baseRef and
// seeded with the resume snapshot, then runs it to its outcome. The stack still wires
// SaveState (checkpoint + taskID), so a second crash mid-resume re-checkpoints from the
// further-advanced tip. cleanup is deferred (it sweeps task/integrate/read — never the
// resume/ pin), so an interrupted resume still preserves the tip.
func runResumedSupervise(ctx context.Context, d serveDeps, goal, taskID, baseRef string, seed *super.ResumeState, approver policy.Approver) (bool, error) {
	// The dollar wall must apply here too. buildStack only mints a ceiling when it is
	// handed a NIL ledger, so passing a bare budget.New() (global ceiling 0 = unlimited)
	// left a serve-restart-resumed multi-agent run with no spend cap at all — unbounded
	// headless spend, unlike every other path. Set the same wall the live serve drive uses.
	ledger := budget.New()
	ledger.SetGlobalCeiling(d.budget)
	bd := serveBuildDeps(d, ledger, approver, goal, taskID)
	bd.baseRef = baseRef
	bd.resume = seed
	stack, err := buildStack(bd)
	if err != nil {
		return false, err
	}
	defer stack.cleanup()
	o, err := buildViaKernel(ctx, stack.loop)
	if err != nil {
		return false, err
	}
	return o.Done, nil
}

// verifyResumeTip rebuilds a throwaway worktree at tipSHA and runs the project verifier
// over it — the I2 re-verify before a resumed run trusts the stored tip as its base. A
// create or verify fault, or a red verdict, is reported as not-verified (the caller
// then degrades to a fresh re-run), never as a panic. Hardened via the same worktree
// + sandbox primitives the live verifier uses.
func verifyResumeTip(ctx context.Context, d serveDeps, tipSHA string) bool {
	verifyCmd := *d.flags.checkCmd
	if verifyCmd == "" {
		verifyCmd = verify.Detect(d.baseRepo)
	}
	wt, err := worktree.CreateFrom(ctx, d.baseRepo, "verify-resume/"+shortID(), "verify-resume-"+shortID(), tipSHA)
	if err != nil {
		d.log.Append(eventlog.Event{Kind: "resume_verify_error", Detail: map[string]any{"tip": tipSHA, "error": err.Error()}})
		return false
	}
	defer func() { _ = wt.Cleanup() }()
	box := selectSandbox(*d.flags.sandboxPref, *d.flags.runtime, *d.flags.image, wt.Path())
	rep, verr := verify.New(box, verifyCmd).Check(ctx)
	if verr != nil {
		d.log.Append(eventlog.Event{Kind: "resume_verify_error", Detail: map[string]any{"tip": tipSHA, "error": verr.Error()}})
		return false
	}
	return rep.Passed
}
