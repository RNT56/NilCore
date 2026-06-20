package swarm

// runner.go — the bounded shard pool (SW-T12).
//
// The Runner maps []Shard onto NilCore's already-race-tested concurrency primitives
// and nothing more. It owns exactly ONE concurrency invariant — the --concurrency cap
// on simultaneously-running shards — and delegates it to the proven pool rather than
// hand-rolling a second one:
//
//   - flat shape ⇒ one scheduler.New(Concurrency) wave: every shard is submitted, the
//     pool runs at most Concurrency at once, and the outcome map is read only AFTER
//     Wait drains. This is the path for independent shards (a list of N reports).
//
//   - dag shape ⇒ spawn.DAGScheduler{MaxConcurrent: Concurrency}: shards with
//     Shard.Deps are released only after their dependencies pass, reusing the exact
//     wave-Kahn release the supervisor uses. A failed dependency skips its dependents
//     transitively (the DAG's own contract).
//
// THE I2 ENFORCEMENT POINT lives INSIDE Fn, not here. The Runner is verifier-agnostic:
// it calls Fn(ctx, shard) and records whatever spawn.Result comes back. The cmd wiring
// supplies an Fn that writes+verifies the shard's artifact and sets Passed/Branch ONLY
// on a green verifier report — so the Runner never sees, and can never be fooled by, a
// self-report. This file's whole job is to run Fn under the cap and aggregate the
// results race-cleanly.
//
// FAILURE ISOLATION. A shard whose Fn errors, returns Passed=false, or PANICS is a
// RECORDED Result, never a pool abort — one exploding shard out of 300 must not lose
// the other 299. The flat path recovers a panic into a failed Result; the dag path
// reuses the DAGScheduler's own panic isolation.

import (
	"context"
	"fmt"
	"sync"

	"nilcore/internal/scheduler"
	"nilcore/internal/spawn"
)

// ShardFunc runs one shard end to end and returns its outcome. It is the seam the cmd
// wiring fills: the supplied function writes the shard's typed artifact, runs the
// per-shard ArtifactVerifier, and sets Result.Passed / Result.Branch ONLY on a green
// report (the I2 ship gate). The Runner treats the returned Result as authoritative —
// it adds no judgment of its own — so ALL of the swarm's "green means verified"
// guarantee is concentrated in this one caller-provided function.
//
// It is exported (ShardFunc) because cmd/nilcore constructs it; the spec's lowercase
// `shardFn` name is the conceptual type, ShardFunc is its exported form.
type ShardFunc func(ctx context.Context, s Shard) spawn.Result

// Runner is the bounded shard pool. Concurrency caps simultaneously-running shards
// (<1 ⇒ 1, matching scheduler.New); Fn runs one shard. The zero value is unusable —
// set Fn. A Runner holds no per-run state, so one Runner may drive many passes
// sequentially (the Controller does exactly that).
type Runner struct {
	Concurrency int
	Fn          ShardFunc
}

// RunPass runs the given shards once under the concurrency cap and returns their
// Results keyed by shard ID. The flat flag selects the topology:
//
//   - flat=true  → one scheduler wave, deps ignored (independent shards).
//   - flat=false → a spawn.DAGScheduler honoring Shard.Deps (ordered shards).
//
// In BOTH paths the outcome map is written under a mutex (flat) or by the DAG's
// single-goroutine fold (dag) and is safe to read only after the pool has drained,
// which RunPass guarantees by returning the fully-folded map. An empty shard slice
// returns an empty (non-nil) map. A nil Fn fails every shard closed rather than
// running an unverified shard.
func (r *Runner) RunPass(ctx context.Context, shards []Shard, flat bool) map[string]spawn.Result {
	if r.Fn == nil {
		// Fail-closed: with no shard function there is no verifier in the loop, so every
		// shard is recorded failed rather than silently "passed". This can only happen
		// from a wiring bug; the explicit Err makes it loud.
		out := make(map[string]spawn.Result, len(shards))
		for i := range shards {
			out[shards[i].ID] = spawn.Result{
				ID:    shards[i].ID,
				State: spawn.StateFailed,
				Err:   fmt.Errorf("swarm runner: no shard function configured"),
			}
		}
		return out
	}
	if flat {
		return r.runFlat(ctx, shards)
	}
	return r.runDAG(ctx, shards)
}

// runFlat submits every shard to ONE scheduler.New(Concurrency) wave. Each task calls
// safeFn (panic-isolated) and stores its Result in the shared map under a mutex; the
// map is read only after Wait drains the pool, so there is no concurrent read against
// the writers. The scheduler's MaxInFlight cap is the single concurrency invariant —
// asserted under -race by the runner test at 300 shards / Concurrency 40.
func (r *Runner) runFlat(ctx context.Context, shards []Shard) map[string]spawn.Result {
	sch := scheduler.New(r.Concurrency)
	results := make(map[string]spawn.Result, len(shards))
	var mu sync.Mutex

	for i := range shards {
		s := shards[i] // capture per-iteration; loop var would alias under concurrency
		sch.Submit(scheduler.Task{
			ID: s.ID,
			Run: func(rctx context.Context) error {
				res := r.safeFn(rctx, s)
				mu.Lock()
				results[s.ID] = res
				mu.Unlock()
				return nil // a shard failure is a recorded Result, not a pool error
			},
		})
	}
	sch.Start(ctx)
	_ = sch.Wait() // every submitted task returns nil; Wait only drains here

	return results
}

// runDAG drives the shards through spawn.DAGScheduler so Shard.Deps are honored: a
// shard runs only after every dependency PASSED, and a failed/skipped dependency
// skips its dependents transitively (the DAG's contract). The DAGScheduler folds each
// node's terminal Result single-goroutine between waves, so the returned map needs no
// extra locking here. RunSub adapts Fn over toSubtask — but Fn needs the full Shard
// (Kind/Pack/Role route the verifier), so we look the Shard back up by id rather than
// reconstruct it from the lossy Subtask.
func (r *Runner) runDAG(ctx context.Context, shards []Shard) map[string]spawn.Result {
	byID := make(map[string]Shard, len(shards))
	subs := make([]spawn.Subtask, 0, len(shards))
	for i := range shards {
		byID[shards[i].ID] = shards[i]
		subs = append(subs, toSubtask(shards[i]))
	}

	dag := spawn.DAGScheduler{
		MaxConcurrent: r.Concurrency,
		// RunSub receives the backend-facing Subtask (Kind/Pack/Role dropped by
		// toSubtask, per I1). We recover the FULL Shard by id to hand Fn the routing it
		// needs — the routing never traveled through the Subtask, preserving the I1
		// boundary, yet the verifier still gets its pack. A subtask with no matching
		// shard (impossible — every sub came from a shard above) is recorded failed.
		RunSub: func(rctx context.Context, st spawn.Subtask) spawn.Result {
			s, ok := byID[st.ID]
			if !ok {
				return spawn.Result{ID: st.ID, Passed: false,
					Err: fmt.Errorf("swarm runner: no shard for subtask %q", st.ID)}
			}
			return r.safeFn(rctx, s)
		},
	}
	return dag.Run(ctx, subs)
}

// safeFn calls Fn with panic isolation so one shard exploding never crashes the pool.
// A panic is converted into a failed Result carrying the recovered value as an error —
// the same discipline spawn's Spawner/DAGScheduler use — so the aggregate map always
// has one terminal Result per shard. The Result's ID is forced to the shard's ID so a
// misbehaving Fn that returns a blank/foreign ID still keys correctly.
func (r *Runner) safeFn(ctx context.Context, s Shard) (res spawn.Result) {
	defer func() {
		if p := recover(); p != nil {
			res = spawn.Result{
				ID:     s.ID,
				Passed: false,
				State:  spawn.StateFailed,
				Err:    fmt.Errorf("swarm runner: shard %q panicked: %v", s.ID, p),
			}
		}
	}()
	res = r.Fn(ctx, s)
	res.ID = s.ID // normalize: the map key and the Result must agree on identity
	return res
}
