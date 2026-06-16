package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"nilcore/internal/backend"
	"nilcore/internal/store"
)

// Checkpoint persists orchestrator task state to the store so an interrupted run
// can resume or fail cleanly on restart, and a SIGTERM can checkpoint before exit
// (P6-T03). State transitions are single UpsertTask writes, so a crash never
// leaves partial state. The orchestrator marks a task "running" at start and
// "done"/"failed" at the end when a Checkpoint is wired.
type Checkpoint struct {
	store *store.Store
}

// NewCheckpoint returns a checkpointer over the store.
func NewCheckpoint(s *store.Store) *Checkpoint { return &Checkpoint{store: s} }

// Begin records a task as running.
func (c *Checkpoint) Begin(ctx context.Context, t backend.Task) error {
	return c.store.UpsertTask(ctx, store.Task{ID: t.ID, Goal: t.Goal, Status: "running"})
}

// Complete records a task's terminal status.
func (c *Checkpoint) Complete(ctx context.Context, taskID, goal string, verified bool) error {
	status := "failed"
	if verified {
		status = "done"
	}
	return c.store.UpsertTask(ctx, store.Task{ID: taskID, Goal: goal, Status: status})
}

// Suspend records a drive's task as self-suspended (the agent set a wake timer). It
// is a non-runnable status that the restart resumer (Resume) skips — it only re-drives
// "running"/"interrupted" — so the suspended drive is NOT re-driven on a restart; the
// wake registry owns resume, which would otherwise double it. One UpsertTask write.
func (c *Checkpoint) Suspend(ctx context.Context, taskID, goal string) error {
	return c.store.UpsertTask(ctx, store.Task{ID: taskID, Goal: goal, Status: "suspended"})
}

// Interrupt marks every running task "interrupted" — the clean SIGTERM checkpoint
// so the next start knows what to resume.
func (c *Checkpoint) Interrupt(ctx context.Context) error {
	running, err := c.store.TasksByStatus(ctx, "running")
	if err != nil {
		return err
	}
	for _, t := range running {
		// Carry Detail through verbatim: a multi-agent run's integration snapshot
		// (tip SHA + per-node state) must survive the SIGTERM checkpoint so Resume
		// can replay merged branches and re-release only un-merged nodes (P5-T03).
		if err := c.store.UpsertTask(ctx, store.Task{ID: t.ID, Goal: t.Goal, Status: "interrupted", Detail: t.Detail}); err != nil {
			return err
		}
	}
	return nil
}

// InFlight returns tasks left running or interrupted by a previous process.
func (c *Checkpoint) InFlight(ctx context.Context) ([]store.Task, error) {
	var out []store.Task
	for _, status := range []string{"running", "interrupted"} {
		ts, err := c.store.TasksByStatus(ctx, status)
		if err != nil {
			return nil, err
		}
		out = append(out, ts...)
	}
	return out, nil
}

// Resume re-runs each in-flight task via run, recording the result. A task that
// errors on resume is marked failed cleanly (the reason is surfaced by run's
// error), so a restart never silently drops or corrupts work.
func (c *Checkpoint) Resume(ctx context.Context, run func(ctx context.Context, t backend.Task) (verified bool, err error)) error {
	inflight, err := c.InFlight(ctx)
	if err != nil {
		return err
	}
	for _, st := range inflight {
		t := backend.Task{ID: st.ID, Goal: st.Goal}
		verified, rerr := run(ctx, t)
		if rerr != nil {
			_ = c.Complete(ctx, st.ID, st.Goal, false)
			continue
		}
		if err := c.Complete(ctx, st.ID, st.Goal, verified); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Multi-agent run-state durability (P5-T03)
//
// The single-task Checkpoint above persists a task's coarse status. A multi-agent
// run needs more: if a SIGTERM lands mid-slice — after some subagent branches have
// been merged into the integration tip but before others are even released — a
// blind re-run would either lose the merged work or double-merge an
// already-integrated branch. Both violate the convergence invariant (CLAUDE.md
// I2: no unverified state is ever the tip; the integrator's reset-on-fail keeps the
// tip green, and a double-merge of a clean branch would either no-op-conflict or
// re-introduce a commit, breaking the maximal-green-prefix guarantee).
//
// So we snapshot two things on every integration checkpoint: the integration-tip
// SHA (the exact verified commit to rebuild the worktree from) and the per-node
// integration state. On restart, RunState.ResumePlan partitions the DAG into
// {already-merged → replay/skip, un-merged ready → re-release}. The snapshot lives
// in store.Task.Detail (opaque JSON, owned here, never parsed by the store), so it
// rides the same single-write status transitions and survives a crash atomically.
// ---------------------------------------------------------------------------

// NodeState is one DAG node's durable integration disposition. It is deliberately
// a small closed set (mirroring spawn.State semantics without importing spawn, so
// durability stays a leaf the orchestrator wiring can snapshot from either layer).
type NodeState string

const (
	// NodePending: not yet released — must be (re-)scheduled on resume.
	NodePending NodeState = "pending"
	// NodeMerged: this node's branch was merged into the integration tip AND the
	// merged tree re-verified green. It is durable: replay rebuilds it from the tip
	// SHA, and it is NEVER re-merged (the double-merge guard).
	NodeMerged NodeState = "merged"
	// NodeFailed: ran but did not pass / its merge was rolled back. Resume re-releases
	// it (a fresh attempt off the rebuilt tip), like a never-released ready node.
	NodeFailed NodeState = "failed"
	// NodeSkipped: a dependency failed/was skipped/cyclic — terminal, never released.
	NodeSkipped NodeState = "skipped"
)

// Node is the durable record of one subtask in the integration DAG: its identity,
// its branch (the commit the integrator folded or will fold), the IDs it depends
// on (so resume can recompute readiness), and its current disposition.
type Node struct {
	ID        string    `json:"id"`
	Branch    string    `json:"branch,omitempty"`
	DependsOn []string  `json:"deps,omitempty"`
	State     NodeState `json:"state"`
}

// RunState is the durable snapshot of a multi-agent integration in progress. TipSHA
// is the verified integration tip the worktree is rebuilt from on resume (the
// integrator's reset-on-fail keeps it green, so rebuilding from it never restores
// an unverified tree). Nodes is the per-node disposition. The zero value is a valid
// empty snapshot. It is JSON for forward compatibility (new fields default-zero on
// an old snapshot) and is the exact payload stored in store.Task.Detail.
type RunState struct {
	TipSHA string `json:"tip_sha,omitempty"`
	Nodes  []Node `json:"nodes,omitempty"`
}

// Marshal serializes the snapshot for store.Task.Detail. It never errors in
// practice (the types are plain JSON), but the error is returned rather than
// swallowed so a caller can halt-gate on a corrupt audit trail.
func (rs RunState) Marshal() (string, error) {
	b, err := json.Marshal(rs)
	if err != nil {
		return "", fmt.Errorf("marshal run state: %w", err)
	}
	return string(b), nil
}

// UnmarshalRunState parses a store.Task.Detail blob. An empty blob is a valid
// empty snapshot (a single task, or a run that never reached integration), so it
// is not an error — the caller simply gets a zero RunState to start fresh from.
func UnmarshalRunState(detail string) (RunState, error) {
	var rs RunState
	if detail == "" {
		return rs, nil
	}
	if err := json.Unmarshal([]byte(detail), &rs); err != nil {
		return RunState{}, fmt.Errorf("unmarshal run state: %w", err)
	}
	return rs, nil
}

// SaveRunState durably records the integration snapshot for a task, preserving the
// task's goal and "running" status. It is one UpsertTask write — the same
// crash-atomic single-statement discipline as Begin/Complete — so a SIGTERM during
// integration leaves either the previous snapshot or this one, never a torn record.
func (c *Checkpoint) SaveRunState(ctx context.Context, taskID, goal string, rs RunState) error {
	detail, err := rs.Marshal()
	if err != nil {
		return err
	}
	return c.store.UpsertTask(ctx, store.Task{ID: taskID, Goal: goal, Status: "running", Detail: detail})
}

// LoadRunState reads back a task's integration snapshot. A task with no snapshot
// (Detail=="") yields a zero RunState — the caller resumes as a fresh run.
func (c *Checkpoint) LoadRunState(ctx context.Context, taskID string) (RunState, error) {
	t, err := c.store.GetTask(ctx, taskID)
	if err != nil {
		return RunState{}, err
	}
	return UnmarshalRunState(t.Detail)
}

// ---------------------------------------------------------------------------
// Conversation durability (C4-T01)
//
// A conversational front-door Session (internal/session) survives restart by
// persisting its BOUNDED work-state — never a raw transcript — through the same
// single-UpsertTask write path as the multi-agent snapshot above. The Session
// owns the work-state⇄JSON mapping; these helpers are the opaque detail carrier
// so internal/session never imports store/backend (it stays a leaf and depends
// only on the narrow interface these two methods satisfy).
//
// A conversation is recorded with a DISTINCT status ("conversation") so it never
// collides with the backend-task resume path: InFlight / Resume look only at
// "running"/"interrupted", so a conversation pointer is never re-run as a coding
// task. The write is one UpsertTask — the same crash-atomic discipline as
// SaveRunState — so a SIGTERM mid-conversation leaves either the prior bounded
// state or this one, never a torn record.
// ---------------------------------------------------------------------------

// ConversationStatus is the store status under which a conversation's bounded
// work-state is recorded — distinct from the backend-task statuses so the resume
// path never mistakes a conversation pointer for a runnable coding task.
const ConversationStatus = "conversation"

// SaveConversation durably records a conversation's bounded work-state (opaque
// JSON the caller owns; the store never parses it) under the conversation ID. It
// is one UpsertTask write, crash-atomic like SaveRunState. id is the conversation
// key (s.ID); goal is a short human label (the work-state goal, for the store's
// goal column); detail is the bounded-state JSON — NEVER a raw transcript.
func (c *Checkpoint) SaveConversation(ctx context.Context, id, goal, detail string) error {
	return c.store.UpsertTask(ctx, store.Task{ID: id, Goal: goal, Status: ConversationStatus, Detail: detail})
}

// LoadConversation reads back a conversation's bounded work-state by ID. found is
// false (with a nil error) when the conversation has no prior record — a fresh
// conversation, restored as the zero work-state. A store miss (sql.ErrNoRows) is
// reported as not-found, not an error, so a first-ever Session restore is clean.
func (c *Checkpoint) LoadConversation(ctx context.Context, id string) (detail string, found bool, err error) {
	t, err := c.store.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("load conversation %q: %w", id, err)
	}
	return t.Detail, true, nil
}

// WakeStatus / wakeDoneStatus are the store statuses for a self-scheduled timer (the
// serve `sleep` tool). A pending wake is a "wake" row keyed wake:<threadID> — distinct
// both from the conversation row (same threadID, status "conversation") and from
// runnable task statuses, so the resume path and the wake poller never cross. Firing
// FLIPS the row to "wake_done" (not a delete) so it leaves an audit trail and is
// excluded from the pending set, and a fired wake can never re-arm. These three
// methods satisfy wake.Store; they reuse UpsertTask/TasksByStatus/GetTask — no new
// store surface — mirroring SaveConversation/LoadConversation.
const (
	WakeStatus     = "wake"
	wakeDoneStatus = "wake_done"
)

func wakeKey(threadID string) string { return "wake:" + threadID }

// SaveWake records (replacing any existing) the single pending wake for threadID with
// the caller's opaque JSON detail. One crash-atomic UpsertTask write.
func (c *Checkpoint) SaveWake(ctx context.Context, threadID, detail string) error {
	return c.store.UpsertTask(ctx, store.Task{ID: wakeKey(threadID), Goal: "wake", Status: WakeStatus, Detail: detail})
}

// LoadWakes returns every currently-armed wake as threadID -> opaque detail. A
// wake_done (fired/disarmed) row is excluded by the status filter, so a fired wake
// never re-arms. Survivors are returned across a restart (durable resume of timers).
func (c *Checkpoint) LoadWakes(ctx context.Context) (map[string]string, error) {
	ts, err := c.store.TasksByStatus(ctx, WakeStatus)
	if err != nil {
		return nil, fmt.Errorf("load wakes: %w", err)
	}
	out := make(map[string]string, len(ts))
	for _, t := range ts {
		out[strings.TrimPrefix(t.ID, "wake:")] = t.Detail
	}
	return out, nil
}

// DisarmWake flips a pending wake to wake_done (keeping the row for audit). Safe when
// none is armed (a store miss is a clean no-op).
func (c *Checkpoint) DisarmWake(ctx context.Context, threadID string) error {
	t, err := c.store.GetTask(ctx, wakeKey(threadID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("disarm wake %q: %w", threadID, err)
	}
	t.Status = wakeDoneStatus
	return c.store.UpsertTask(ctx, t)
}

// ResumePlan is the partition Resume hands the integration layer after a restart.
// Merged are the node IDs whose branches are already on the rebuilt tip and must
// NOT be re-merged (the double-merge guard). Release are the node IDs to schedule
// again: un-merged nodes (pending/failed) whose every dependency is Merged — i.e.
// the ones the DAG would now consider ready off the rebuilt tip. Skip are terminal
// nodes (skipped, or downstream of a failure that never became ready) that resume
// leaves alone. Every node lands in exactly one bucket.
type ResumePlan struct {
	TipSHA  string
	Merged  []string
	Release []string
	Skip    []string
}

// ResumePlan computes, from a durable snapshot, what to replay versus re-release.
// It is the heart of the no-work-lost / no-double-merge guarantee:
//
//   - A Merged node is replayed from the tip (skip its re-merge).
//   - A pending/failed node is re-released ONLY if every dependency is Merged —
//     so it is coded off the same rebuilt tip the live DAG would have given it,
//     and a node whose deps are not yet on the tip waits (it is neither released
//     nor skipped this pass; the live DAG releases it once its deps merge).
//   - A skipped node, or one blocked by a non-merged/failed dependency, is left
//     to the live scheduler as not-yet-ready (reported under Skip for visibility).
//
// The computation is a single pass over a snapshot the writer already ordered (the
// integrator merges in topological order), so it needs no fixpoint: a dependency is
// Merged or it is not, and readiness is monotone in "all deps merged".
func (rs RunState) ResumePlan() ResumePlan {
	merged := make(map[string]bool, len(rs.Nodes))
	for _, n := range rs.Nodes {
		if n.State == NodeMerged {
			merged[n.ID] = true
		}
	}

	plan := ResumePlan{TipSHA: rs.TipSHA}
	for _, n := range rs.Nodes {
		switch n.State {
		case NodeMerged:
			plan.Merged = append(plan.Merged, n.ID)
		case NodeSkipped:
			plan.Skip = append(plan.Skip, n.ID)
		default: // pending or failed: re-release iff every dep is already merged
			ready := true
			for _, dep := range n.DependsOn {
				if !merged[dep] {
					ready = false
					break
				}
			}
			if ready {
				plan.Release = append(plan.Release, n.ID)
			} else {
				plan.Skip = append(plan.Skip, n.ID)
			}
		}
	}
	return plan
}
