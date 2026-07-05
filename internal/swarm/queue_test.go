package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/requeue"
	"nilcore/internal/store"
)

// jsonUnmarshal is a tiny test helper that fails the test on a parse error rather than
// returning it, keeping the assertion sites terse.
func jsonUnmarshal(t *testing.T, blob string, v any) error {
	t.Helper()
	if err := json.Unmarshal([]byte(blob), v); err != nil {
		t.Fatalf("unmarshal %q: %v", blob, err)
	}
	return nil
}

// openStore opens a fresh temp-file SQLite store for one test. A file (not :memory:)
// matches production and survives the single-conn pool the store enforces.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "swarm.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestEnqueueRoundTripsState asserts Enqueue persists SwarmState (with the Ledger) into
// the run row and that LoadState reads it back field-for-field.
func TestEnqueueRoundTripsState(t *testing.T) {
	ctx := context.Background()
	q := NewQueue(openStore(t), nil, "run1")

	st := SwarmState{
		RunID:  "run1",
		Goal:   "build the report",
		Preset: "research",
		Pass:   3,
		Ledger: requeue.Ledger{MaxAttempts: 2, Attempts: map[string]int{"swarm-run1-0/c1": 1}},
		TipSHA: "deadbeef",
	}
	shards := []Shard{
		{ID: "swarm-run1-0", Goal: "g0", Kind: artifact.KindReport, Pack: "finance", State: ShardQueued},
	}
	if err := q.Enqueue(ctx, st, shards); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := q.LoadState(ctx)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if got.Goal != st.Goal || got.Preset != st.Preset || got.Pass != st.Pass || got.TipSHA != st.TipSHA {
		t.Errorf("state mismatch: got %+v want %+v", got, st)
	}
	if got.Ledger.MaxAttempts != 2 || got.Ledger.Attempts["swarm-run1-0/c1"] != 1 {
		t.Errorf("ledger not round-tripped: %+v", got.Ledger)
	}
}

// TestMarkFullDetailRMWPreservesGreen is the regression guard for the UpsertTask
// Detail-overwrite trap: a status re-mark must NOT wipe a prior shard's blob. Write a
// shard green (Green=true persisted), then re-Mark it with a different status, and
// assert the persisted refs survive (the RMW read the old blob before writing).
func TestMarkFullDetailRMWPreservesGreen(t *testing.T) {
	ctx := context.Background()
	q := NewQueue(openStore(t), nil, "run1")

	s := Shard{
		ID:      "swarm-run1-0",
		Goal:    "g0",
		Input:   "seed",
		Kind:    artifact.KindReport,
		Pack:    "finance",
		Role:    "researcher",
		Deps:    []string{"swarm-run1-x"},
		Branch:  "task/swarm-run1-0",
		Attempt: 1,
		State:   ShardPassed, // green: Green=true is persisted
	}
	if err := q.Mark(ctx, s); err != nil {
		t.Fatalf("mark passed: %v", err)
	}

	// Confirm the durable row is green and carries the refs.
	row, err := q.store.GetTask(ctx, s.ID)
	if err != nil {
		t.Fatalf("get after pass: %v", err)
	}
	if row.Status != StatusPassed {
		t.Errorf("status = %q, want %q", row.Status, StatusPassed)
	}

	// Now re-mark the SAME shard with a new status but a shard value that still says
	// passed — the RMW must preserve the blob (Branch/Pack/etc.) and the Green bit.
	s.State = ShardPassed
	if err := q.Mark(ctx, s); err != nil {
		t.Fatalf("re-mark: %v", err)
	}

	got, err := q.store.GetTask(ctx, s.ID)
	if err != nil {
		t.Fatalf("get after re-mark: %v", err)
	}
	rebuilt, err := shardFromRow(got)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if rebuilt.Branch != "task/swarm-run1-0" || rebuilt.Pack != "finance" || rebuilt.Role != "researcher" {
		t.Errorf("refs wiped by re-mark: %+v", rebuilt)
	}
	if rebuilt.State != ShardPassed {
		t.Errorf("green not preserved: state = %q", rebuilt.State)
	}
}

// TestMarkNeverWipesPriorGreenOnStatusChange exercises the precise overwrite-regression
// the spec calls out: write green=true via a passed Mark, then Mark a DIFFERENT logical
// update (a fresh shard value missing some refs) and confirm the persisted Green field
// is not silently cleared because the writer forgot to set it. We do this by marking the
// shard failed and back, asserting the durable Detail is always a full record.
func TestMarkRMWAcrossStatusTransition(t *testing.T) {
	ctx := context.Background()
	q := NewQueue(openStore(t), nil, "run1")

	base := Shard{ID: "swarm-run1-0", Goal: "g", Pack: "p", Branch: "b", State: ShardPassed}
	if err := q.Mark(ctx, base); err != nil {
		t.Fatalf("mark pass: %v", err)
	}
	// Read the raw blob; Green must be true.
	row, _ := q.store.GetTask(ctx, base.ID)
	var d shardDetail
	if row.Detail != "" {
		_ = jsonUnmarshal(t, row.Detail, &d)
	}
	if !d.Green {
		t.Fatalf("expected Green=true after pass, got %+v", d)
	}

	// Transition to failed: Green must now be false, but Pack/Branch survive the RMW.
	base.State = ShardFailed
	if err := q.Mark(ctx, base); err != nil {
		t.Fatalf("mark fail: %v", err)
	}
	row, _ = q.store.GetTask(ctx, base.ID)
	_ = jsonUnmarshal(t, row.Detail, &d)
	if d.Green {
		t.Errorf("Green should be false after fail")
	}
	if d.Pack != "p" || d.Branch != "b" {
		t.Errorf("refs lost across transition: %+v", d)
	}
}

// TestNamespaceIsolationFromNative asserts a swarm-running row never appears in a native
// "running" status sweep — the namespace is distinct so native resume never re-drives a
// swarm shard.
func TestNamespaceIsolationFromNative(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	q := NewQueue(st, nil, "run1")

	if err := q.Mark(ctx, Shard{ID: "swarm-run1-0", Goal: "g", State: ShardRunning}); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	// The native sweep filters on the bare "running" status.
	native, err := st.TasksByStatus(ctx, "running")
	if err != nil {
		t.Fatalf("native running: %v", err)
	}
	if len(native) != 0 {
		t.Errorf("native sweep saw %d swarm rows, want 0", len(native))
	}
	// The swarm-running status finds it.
	swarmRows, err := st.TasksByStatus(ctx, StatusRunning)
	if err != nil {
		t.Fatalf("swarm running: %v", err)
	}
	if len(swarmRows) != 1 {
		t.Errorf("swarm-running rows = %d, want 1", len(swarmRows))
	}
}

// TestRunIsolationByPrefix asserts two runs sharing one store do not see each other's
// shards through ShardsByRun.
func TestRunIsolationByPrefix(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	q1 := NewQueue(st, nil, "runA")
	q2 := NewQueue(st, nil, "runB")

	if err := q1.Mark(ctx, Shard{ID: "swarm-runA-0", Goal: "a", State: ShardFailed}); err != nil {
		t.Fatal(err)
	}
	if err := q2.Mark(ctx, Shard{ID: "swarm-runB-0", Goal: "b", State: ShardFailed}); err != nil {
		t.Fatal(err)
	}

	a, err := q1.ShardsByRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || a[0].ID != "swarm-runA-0" {
		t.Errorf("runA shards = %+v, want only swarm-runA-0", a)
	}
	b, err := q2.ShardsByRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 1 || b[0].ID != "swarm-runB-0" {
		t.Errorf("runB shards = %+v, want only swarm-runB-0", b)
	}
}

// TestFailedReturnsEligibleOnly asserts Failed returns only failed shards whose artifact
// retains retry budget against the Ledger; an exhausted shard is dropped.
func TestFailedReturnsEligibleOnly(t *testing.T) {
	ctx := context.Background()
	q := NewQueue(openStore(t), nil, "run1")

	// Two failed shards. The Ledger gives shard 0 budget (attempt 1 < max 2) and marks
	// shard 1 exhausted (attempt 2 == max 2).
	for _, id := range []string{"swarm-run1-0", "swarm-run1-1"} {
		if err := q.Mark(ctx, Shard{ID: id, Goal: "g", State: ShardFailed}); err != nil {
			t.Fatal(err)
		}
	}
	led := &requeue.Ledger{
		MaxAttempts: 2,
		Attempts: map[string]int{
			"swarm-run1-0/c1": 1, // budget remains
			"swarm-run1-1/c1": 2, // exhausted
		},
	}
	got, err := q.Failed(ctx, led)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "swarm-run1-0" {
		t.Errorf("Failed eligible = %+v, want only swarm-run1-0", got)
	}
}

// TestFailedNilLedgerReturnsAll asserts a nil Ledger skips the budget gate and returns
// every failed shard (the Controller then intersects with a fresh Scan).
func TestFailedNilLedgerReturnsAll(t *testing.T) {
	ctx := context.Background()
	q := NewQueue(openStore(t), nil, "run1")
	for _, id := range []string{"swarm-run1-0", "swarm-run1-1"} {
		if err := q.Mark(ctx, Shard{ID: id, Goal: "g", State: ShardFailed}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := q.Failed(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("nil-ledger Failed = %d shards, want 2", len(got))
	}
}

// TestInFlightSwarmFindsRunRow asserts the run row is discoverable as in-flight while a
// native task in another status is not surfaced by InFlightSwarm.
func TestInFlightSwarmFindsRunRow(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	q := NewQueue(st, nil, "run1")
	if err := q.SaveState(ctx, SwarmState{RunID: "run1", Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	// A native task in "running" must not appear in InFlightSwarm.
	if err := st.UpsertTask(ctx, store.Task{ID: "native-1", Goal: "n", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	rows, err := q.InFlightSwarm(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "swarm-run1" {
		t.Errorf("InFlightSwarm = %+v, want only the run row swarm-run1", rows)
	}
}

// TestMarkConvergedRemovesFromInFlight asserts that a converged run's row is moved to a
// terminal status so InFlightSwarm no longer discovers it — a later --resume must not
// re-adopt a finished run. LoadState still round-trips its state (the row is preserved).
func TestMarkConvergedRemovesFromInFlight(t *testing.T) {
	ctx := context.Background()
	q := NewQueue(openStore(t), nil, "run1")
	st := SwarmState{RunID: "run1", Goal: "g", Pass: 3, TipSHA: "abc"}
	if err := q.SaveState(ctx, st); err != nil {
		t.Fatal(err)
	}
	// While in flight, the run is discoverable.
	rows, err := q.InFlightSwarm(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("in-flight before converge = %d rows, want 1", len(rows))
	}
	// Converge: the row moves to the terminal status.
	if err := q.MarkConverged(ctx, st); err != nil {
		t.Fatalf("MarkConverged: %v", err)
	}
	rows, err = q.InFlightSwarm(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("in-flight after converge = %d rows, want 0 (must not re-adopt a finished run)", len(rows))
	}
	// The state is preserved (the row still exists, just terminal).
	got, err := q.LoadState(ctx)
	if err != nil {
		t.Fatalf("LoadState after converge: %v", err)
	}
	if got.Pass != 3 || got.TipSHA != "abc" {
		t.Errorf("LoadState after converge = %+v, want Pass=3 TipSHA=abc preserved", got)
	}
}

// TestLoadStateMissingRun asserts a missing run row surfaces sql.ErrNoRows so the caller
// distinguishes "no such run" from a corrupt blob.
func TestLoadStateMissingRun(t *testing.T) {
	q := NewQueue(openStore(t), nil, "absent")
	_, err := q.LoadState(context.Background())
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("LoadState on missing run = %v, want sql.ErrNoRows", err)
	}
}
