package swarm

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nilcore/internal/spawn"
)

// TestRunPassFlatConcurrencyCap drives 300 shards through a flat pass at Concurrency 40
// and asserts (a) every shard ran exactly once, (b) the peak in-flight never EXCEEDED the
// cap, and (c) the peak actually REACHED the cap. The third is the discriminating part
// (MINOR #15): an instant Fn finishes so fast that the scheduler rarely has cap shards
// running at once, so the old test could pass even if the scheduler ran far fewer in
// parallel. Here a FAN-IN BARRIER parks every shard until exactly `cap` are simultaneously
// in flight, then releases them — so the high-water mark is forced to the cap and the
// assertion is "peak == cap", not merely "peak <= cap".
//
// The barrier is safe against the n>cap shape: the scheduler runs at most `cap` shards at
// once, so exactly `cap` goroutines park on the first wave; the cap-th arrival closes the
// gate, releasing that wave; each subsequent wave re-parks but the gate is already closed,
// so later waves pass straight through (the peak was already recorded at the cap). Run
// under -race this stays the runner's core concurrency proof. A watchdog context guards
// against a hang if the barrier logic were ever wrong (it would otherwise deadlock).
func TestRunPassFlatConcurrencyCap(t *testing.T) {
	const n, capN = 300, 40
	var inFlight, peak, ran atomic.Int64

	// gate closes once `cap` shards are simultaneously parked; closing it releases the
	// saturated wave. `arrived` counts shards that have reached the barrier this wave.
	gate := make(chan struct{})
	var once sync.Once
	var arrived atomic.Int64

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fn := func(fctx context.Context, s Shard) spawn.Result {
		cur := inFlight.Add(1)
		for { // record the high-water mark race-cleanly
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		// Fan-in barrier: the cap-th simultaneously-parked shard closes the gate, which
		// releases the whole saturated wave at once. This guarantees `cap` shards are in
		// flight together, forcing peak to reach the cap.
		if arrived.Add(1) >= capN {
			once.Do(func() { close(gate) })
		}
		select {
		case <-gate:
		case <-fctx.Done(): // watchdog: never hang the suite if the barrier mislogic'd
		}

		ran.Add(1)
		inFlight.Add(-1)
		return spawn.Result{ID: s.ID, Passed: true, Branch: "b/" + s.ID}
	}

	r := &Runner{Concurrency: capN, Fn: fn}
	shards := make([]Shard, n)
	for i := range shards {
		shards[i] = Shard{ID: fmt.Sprintf("swarm/run/%d", i), Goal: "g", State: ShardQueued}
	}

	results := r.RunPass(ctx, shards, true)

	if err := ctx.Err(); err != nil {
		t.Fatalf("run timed out (barrier never saturated to cap %d): %v", capN, err)
	}
	if got := ran.Load(); got != n {
		t.Errorf("ran = %d, want %d", got, n)
	}
	if len(results) != n {
		t.Errorf("results = %d, want %d", len(results), n)
	}
	if p := peak.Load(); p > capN {
		t.Errorf("peak in-flight = %d, exceeded cap %d", p, capN)
	}
	// The discriminating assertion: the barrier forced a fully-saturated wave, so the peak
	// must have REACHED the cap (within [cap-ε, cap]; here exactly cap by construction).
	if p := peak.Load(); p < capN {
		t.Errorf("peak in-flight = %d, never reached cap %d — the pool ran under-parallel", p, capN)
	}
	for i := range shards {
		if !results[shards[i].ID].Passed {
			t.Errorf("shard %s not recorded passed", shards[i].ID)
		}
	}
}

// TestRunPassRecordsPanicNotFatal asserts a shard whose Fn panics is recorded as a
// failed Result, never crashing the pool — the other shards still complete.
func TestRunPassRecordsPanicNotFatal(t *testing.T) {
	fn := func(ctx context.Context, s Shard) spawn.Result {
		if s.ID == "swarm/run/1" {
			panic("boom")
		}
		return spawn.Result{ID: s.ID, Passed: true}
	}
	r := &Runner{Concurrency: 4, Fn: fn}
	shards := []Shard{
		{ID: "swarm/run/0", State: ShardQueued},
		{ID: "swarm/run/1", State: ShardQueued},
		{ID: "swarm/run/2", State: ShardQueued},
	}
	results := r.RunPass(context.Background(), shards, true)

	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 (panic must not lose siblings)", len(results))
	}
	if results["swarm/run/1"].Passed {
		t.Errorf("panicking shard recorded passed; want failed")
	}
	if results["swarm/run/1"].Err == nil {
		t.Errorf("panicking shard has no Err recorded")
	}
	if !results["swarm/run/0"].Passed || !results["swarm/run/2"].Passed {
		t.Errorf("sibling shards lost: %+v", results)
	}
}

// TestRunPassRecordsErrNotFatal asserts an Fn returning Passed=false / Err set is a
// recorded Result, not a pool abort.
func TestRunPassRecordsErrNotFatal(t *testing.T) {
	fn := func(ctx context.Context, s Shard) spawn.Result {
		if s.ID == "swarm/run/x" {
			return spawn.Result{ID: s.ID, Passed: false, Err: fmt.Errorf("red")}
		}
		return spawn.Result{ID: s.ID, Passed: true}
	}
	r := &Runner{Concurrency: 2, Fn: fn}
	shards := []Shard{{ID: "swarm/run/x", State: ShardQueued}, {ID: "swarm/run/y", State: ShardQueued}}
	results := r.RunPass(context.Background(), shards, true)
	if results["swarm/run/x"].Passed || results["swarm/run/x"].Err == nil {
		t.Errorf("erroring shard should be recorded failed with Err: %+v", results["swarm/run/x"])
	}
	if !results["swarm/run/y"].Passed {
		t.Errorf("healthy sibling lost")
	}
}

// TestRunPassNilFnFailsClosed asserts a Runner with no Fn fails every shard closed
// rather than running an unverified (silently passed) shard.
func TestRunPassNilFnFailsClosed(t *testing.T) {
	r := &Runner{Concurrency: 2}
	shards := []Shard{{ID: "swarm/run/0", State: ShardQueued}}
	results := r.RunPass(context.Background(), shards, true)
	if results["swarm/run/0"].Passed {
		t.Errorf("nil-Fn shard must not be passed")
	}
	if results["swarm/run/0"].Err == nil {
		t.Errorf("nil-Fn shard must carry an explicit Err")
	}
}

// TestRunPassDAGOrdering asserts the dag path honors Shard.Deps: B (depends on A) runs
// only after A passes, and a FAILED A skips B entirely.
func TestRunPassDAGOrdering(t *testing.T) {
	t.Run("A passes releases B", func(t *testing.T) {
		var aDone atomic.Bool
		var bSawADone atomic.Bool
		fn := func(ctx context.Context, s Shard) spawn.Result {
			switch s.ID {
			case "swarm/run/A":
				aDone.Store(true)
				return spawn.Result{ID: s.ID, Passed: true}
			case "swarm/run/B":
				bSawADone.Store(aDone.Load())
				return spawn.Result{ID: s.ID, Passed: true}
			}
			return spawn.Result{ID: s.ID, Passed: true}
		}
		r := &Runner{Concurrency: 4, Fn: fn}
		shards := []Shard{
			{ID: "swarm/run/A", State: ShardQueued},
			{ID: "swarm/run/B", Deps: []string{"swarm/run/A"}, State: ShardQueued},
		}
		results := r.RunPass(context.Background(), shards, false)
		if !results["swarm/run/B"].Passed {
			t.Errorf("B should have passed")
		}
		if !bSawADone.Load() {
			t.Errorf("B ran before A completed — DAG ordering violated")
		}
	})

	t.Run("failed A skips B", func(t *testing.T) {
		var bRan atomic.Bool
		fn := func(ctx context.Context, s Shard) spawn.Result {
			if s.ID == "swarm/run/B" {
				bRan.Store(true)
			}
			if s.ID == "swarm/run/A" {
				return spawn.Result{ID: s.ID, Passed: false, Err: fmt.Errorf("A red")}
			}
			return spawn.Result{ID: s.ID, Passed: true}
		}
		r := &Runner{Concurrency: 4, Fn: fn}
		shards := []Shard{
			{ID: "swarm/run/A", State: ShardQueued},
			{ID: "swarm/run/B", Deps: []string{"swarm/run/A"}, State: ShardQueued},
		}
		results := r.RunPass(context.Background(), shards, false)
		if bRan.Load() {
			t.Errorf("B's Fn ran despite A failing — dependent must be skipped")
		}
		if results["swarm/run/B"].State != spawn.StateSkipped {
			t.Errorf("B state = %q, want skipped", results["swarm/run/B"].State)
		}
	})
}
