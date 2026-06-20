package swarm

import (
	"testing"

	"nilcore/internal/artifact"
)

// toSubtask must carry only the backend-facing identity (ID/Goal/Deps) and DROP
// every harness-routing field. This is the executable I1 boundary check: the
// backend unit can never observe the swarm's pack/tier/kind choices.
func TestToSubtaskDropsRoutingFields(t *testing.T) {
	s := Shard{
		ID:      "swarm/run42/3",
		Input:   "seed material the worker should NOT have routing for",
		Goal:    "produce the Q4 revenue report",
		Kind:    artifact.KindReport,
		Pack:    "finance",
		Role:    "researcher",
		Tier:    "strong",
		Deps:    []string{"swarm/run42/1", "swarm/run42/2"},
		State:   ShardRunning,
		Attempt: 2,
		Branch:  "task/swarm-run42-3",
	}

	sub := toSubtask(s)

	if sub.ID != s.ID {
		t.Errorf("ID = %q, want %q", sub.ID, s.ID)
	}
	if sub.Goal != s.Goal {
		t.Errorf("Goal = %q, want %q", sub.Goal, s.Goal)
	}
	if len(sub.DependsOn) != len(s.Deps) {
		t.Fatalf("DependsOn = %v, want %v", sub.DependsOn, s.Deps)
	}
	for i := range s.Deps {
		if sub.DependsOn[i] != s.Deps[i] {
			t.Errorf("DependsOn[%d] = %q, want %q", i, sub.DependsOn[i], s.Deps[i])
		}
	}

	// spawn.Subtask has no Kind/Pack/Tier/Role/Input/State/Attempt/Branch fields
	// at all — the drop is enforced by the type, not by zeroing. Assert the
	// routing data is structurally absent by confirming the subtask cannot echo
	// any of the shard's routing strings through its only string-bearing fields.
	for _, leak := range []string{string(s.Kind), s.Pack, s.Tier, s.Role, s.Input, s.Branch} {
		if leak == "" {
			continue
		}
		if sub.ID == leak || sub.Goal == leak {
			t.Errorf("routing value %q leaked into the subtask", leak)
		}
	}
}

// toSubtask must copy the Deps slice so a later mutation of the shard cannot
// reach into an already-constructed subtask handed to spawn.
func TestToSubtaskCopiesDeps(t *testing.T) {
	s := Shard{ID: "swarm/run1/0", Goal: "g", Deps: []string{"swarm/run1/x"}}
	sub := toSubtask(s)
	s.Deps[0] = "MUTATED"
	if sub.DependsOn[0] != "swarm/run1/x" {
		t.Errorf("subtask deps aliased the shard slice: got %q", sub.DependsOn[0])
	}
}

// toSubtask on a shard with no deps must yield a nil (not empty-but-aliased)
// DependsOn, matching spawn.FromPlan's convention.
func TestToSubtaskNilDeps(t *testing.T) {
	sub := toSubtask(Shard{ID: "swarm/run1/0", Goal: "g"})
	if sub.DependsOn != nil {
		t.Errorf("DependsOn = %v, want nil", sub.DependsOn)
	}
}

// ShardState is a closed set; this guards the canonical string values so the
// durable queue's status namespace (SW-T10) stays in sync.
func TestShardStateValues(t *testing.T) {
	cases := map[ShardState]string{
		ShardQueued:    "queued",
		ShardRunning:   "running",
		ShardPassed:    "passed",
		ShardFailed:    "failed",
		ShardExhausted: "exhausted",
		ShardSkipped:   "skipped",
	}
	for st, want := range cases {
		if string(st) != want {
			t.Errorf("ShardState %v = %q, want %q", st, string(st), want)
		}
	}
}
