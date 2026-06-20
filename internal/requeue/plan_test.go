package requeue

// plan_test.go — hermetic, table-driven coverage for the planner and resolver
// (P11-T21). No filesystem, no network: Plan/Resolve are pure transforms over
// in-memory Worklists, so the tests construct Units directly.

import (
	"reflect"
	"strings"
	"testing"

	"nilcore/internal/artifact"
)

// unit is a terse Unit constructor for the table tests below.
func unit(artID, claimID string, st artifact.Status) Unit {
	return Unit{ArtifactID: artID, ClaimID: claimID, Field: "f-" + claimID, Status: st}
}

func TestPlanResolve(t *testing.T) {
	t.Run("plan groups red claims into one subtask per artifact", func(t *testing.T) {
		led := &Ledger{MaxAttempts: 3}
		wl := Worklist{Units: []Unit{
			unit("art-a", "c1", artifact.StatusFail),
			unit("art-a", "c2", artifact.StatusStale),
			unit("art-b", "c3", artifact.StatusUnverifiable),
		}}
		got := Plan(wl, led, "attempt-0")
		if len(got) != 2 {
			t.Fatalf("want 2 subtasks (one per artifact), got %d: %+v", len(got), got)
		}
		// Deterministic sorted order: art-a before art-b.
		if got[0].ID != "requeue-art-a" || got[1].ID != "requeue-art-b" {
			t.Fatalf("subtask ids/order wrong: %q, %q", got[0].ID, got[1].ID)
		}
		// art-a's Goal names BOTH its red claims; UnitKeys lists both.
		if !strings.Contains(got[0].Goal, "c1") || !strings.Contains(got[0].Goal, "c2") {
			t.Fatalf("art-a goal must name c1 and c2: %q", got[0].Goal)
		}
		wantKeys := []string{"art-a/c1", "art-a/c2"}
		if !reflect.DeepEqual(got[0].UnitKeys, wantKeys) {
			t.Fatalf("art-a UnitKeys = %v, want %v", got[0].UnitKeys, wantKeys)
		}
		// ContinueFrom is the prior attempt id supplied to Plan.
		if got[0].ContinueFrom != "attempt-0" || got[1].ContinueFrom != "attempt-0" {
			t.Fatalf("ContinueFrom not set to prior attempt: %q, %q", got[0].ContinueFrom, got[1].ContinueFrom)
		}
	})

	t.Run("plan splits an artifact by owner", func(t *testing.T) {
		led := &Ledger{MaxAttempts: 2}
		ua := unit("art-a", "c1", artifact.StatusFail)
		ua.OwnerSubagent = "owner-x"
		ub := unit("art-a", "c2", artifact.StatusFail)
		ub.OwnerSubagent = "owner-y"
		got := Plan(Worklist{Units: []Unit{ua, ub}}, led, "base")
		if len(got) != 2 {
			t.Fatalf("distinct owners must split into 2 subtasks, got %d", len(got))
		}
	})

	t.Run("plan excludes exhausted units", func(t *testing.T) {
		// MaxAttempts 1 with c1 already at 1 attempt ⇒ c1 exhausted, excluded.
		led := &Ledger{MaxAttempts: 1, Attempts: map[string]int{"art-a/c1": 1}}
		wl := Worklist{Units: []Unit{
			unit("art-a", "c1", artifact.StatusFail), // exhausted
			unit("art-a", "c2", artifact.StatusFail), // budget remains
		}}
		got := Plan(wl, led, "base")
		if len(got) != 1 {
			t.Fatalf("want 1 subtask after excluding exhausted, got %d", len(got))
		}
		if strings.Contains(got[0].Goal, "c1") {
			t.Fatalf("exhausted c1 must be excluded from goal: %q", got[0].Goal)
		}
		if !reflect.DeepEqual(got[0].UnitKeys, []string{"art-a/c2"}) {
			t.Fatalf("UnitKeys should hold only c2: %v", got[0].UnitKeys)
		}
	})

	t.Run("plan yields zero subtasks on empty worklist", func(t *testing.T) {
		if got := Plan(Worklist{}, &Ledger{MaxAttempts: 3}, "base"); len(got) != 0 {
			t.Fatalf("empty worklist must plan zero subtasks, got %d", len(got))
		}
	})

	t.Run("plan yields zero subtasks when all units exhausted", func(t *testing.T) {
		// MaxAttempts==0 ⇒ every unit exhausted at attempt 0 (requeue disabled).
		wl := Worklist{Units: []Unit{unit("art-a", "c1", artifact.StatusFail)}}
		if got := Plan(wl, &Ledger{MaxAttempts: 0}, "base"); len(got) != 0 {
			t.Fatalf("all-exhausted worklist must plan zero subtasks, got %d", len(got))
		}
	})
}

func TestResolve(t *testing.T) {
	t.Run("green-flip: resolved when dropped from after", func(t *testing.T) {
		led := &Ledger{MaxAttempts: 3}
		before := Worklist{Units: []Unit{
			unit("art-a", "c1", artifact.StatusFail),
			unit("art-a", "c2", artifact.StatusFail),
		}}
		// c1 passed (absent from after); c2 still red.
		after := Worklist{Units: []Unit{unit("art-a", "c2", artifact.StatusFail)}}
		resolved, stillFailed, exhausted := Resolve(before, after, led)
		if len(resolved) != 1 || resolved[0].ClaimID != "c1" {
			t.Fatalf("c1 should be resolved, got %+v", resolved)
		}
		if len(stillFailed) != 1 || stillFailed[0].ClaimID != "c2" {
			t.Fatalf("c2 should be stillFailed, got %+v", stillFailed)
		}
		if len(exhausted) != 0 {
			t.Fatalf("nothing exhausted yet (1/3), got %+v", exhausted)
		}
		// c2 was Bump'd once.
		if stillFailed[0].Attempt != 1 {
			t.Fatalf("c2 attempt should be 1 after Bump, got %d", stillFailed[0].Attempt)
		}
		if led.attemptFor(unit("art-a", "c2", artifact.StatusFail)) != 1 {
			t.Fatalf("ledger should record c2 attempt 1")
		}
		// Loop continues: 1 stillFailed > 0 exhausted.
		if !ShouldContinue(stillFailed, exhausted) {
			t.Fatalf("loop should continue with a non-exhausted red unit")
		}
	})

	t.Run("stay-red across a round without exhausting", func(t *testing.T) {
		led := &Ledger{MaxAttempts: 5}
		before := Worklist{Units: []Unit{unit("art-a", "c1", artifact.StatusFail)}}
		after := Worklist{Units: []Unit{unit("art-a", "c1", artifact.StatusFail)}}
		resolved, stillFailed, exhausted := Resolve(before, after, led)
		if len(resolved) != 0 || len(stillFailed) != 1 || len(exhausted) != 0 {
			t.Fatalf("stay-red: want 0/1/0, got %d/%d/%d", len(resolved), len(stillFailed), len(exhausted))
		}
		if !ShouldContinue(stillFailed, exhausted) {
			t.Fatalf("should still continue (budget remains)")
		}
	})

	t.Run("ceiling: stillFailed hitting MaxAttempts is exhausted and stops the loop", func(t *testing.T) {
		// c1 already at 2 attempts, MaxAttempts 3 ⇒ this round Bumps to 3 ⇒ exhausted.
		led := &Ledger{MaxAttempts: 3, Attempts: map[string]int{"art-a/c1": 2}}
		before := Worklist{Units: []Unit{unit("art-a", "c1", artifact.StatusFail)}}
		after := Worklist{Units: []Unit{unit("art-a", "c1", artifact.StatusFail)}}
		resolved, stillFailed, exhausted := Resolve(before, after, led)
		if len(resolved) != 0 || len(stillFailed) != 1 || len(exhausted) != 1 {
			t.Fatalf("ceiling: want 0/1/1, got %d/%d/%d", len(resolved), len(stillFailed), len(exhausted))
		}
		if stillFailed[0].Attempt != 3 {
			t.Fatalf("c1 should be at attempt 3, got %d", stillFailed[0].Attempt)
		}
		// Loop STOPS: the only still-red unit is exhausted.
		if ShouldContinue(stillFailed, exhausted) {
			t.Fatalf("loop must stop when every still-red unit is exhausted")
		}
	})

	t.Run("all resolved stops the loop", func(t *testing.T) {
		led := &Ledger{MaxAttempts: 3}
		before := Worklist{Units: []Unit{unit("art-a", "c1", artifact.StatusFail)}}
		after := Worklist{} // verifier passed everything
		resolved, stillFailed, exhausted := Resolve(before, after, led)
		if len(resolved) != 1 || len(stillFailed) != 0 || len(exhausted) != 0 {
			t.Fatalf("all-resolved: want 1/0/0, got %d/%d/%d", len(resolved), len(stillFailed), len(exhausted))
		}
		if ShouldContinue(stillFailed, exhausted) {
			t.Fatalf("loop must stop when nothing is still failing")
		}
	})
}
