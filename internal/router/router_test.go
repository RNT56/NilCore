package router

import (
	"context"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		goal string
		want Preset
	}{
		// Run — the cheapest default for an ordinary single task.
		{"fix the nil-pointer panic in auth.go", Run},
		{"add a -verbose flag to the CLI", Run},
		{"rename the User struct to Account", Run},
		{"", Run},
		// Build — whole-project / scaffold shapes.
		{"build a REST API for todo items", Build},
		{"scaffold a new service with health checks", Build},
		{"create a new project from scratch", Build},
		{"bootstrap a CLI app with cobra", Build},
		// Swarm — breadth / parallel objectives.
		{"add a unit test to every package", Swarm},
		{"fix the lint errors in parallel across all modules", Swarm},
		{"audit the codebase for SQL injection", Swarm},
		{"run a swarm to migrate each of the handlers", Swarm},
		// Order: a breadth signal wins over a build signal.
		{"scaffold a new service for every region", Swarm},
	}
	for _, c := range cases {
		if got := Classify(c.goal); got != c.want {
			t.Errorf("Classify(%q) = %q, want %q", c.goal, got, c.want)
		}
	}
}

// stubOracle returns a fixed pick + opinion flag, recording what it was asked.
type stubOracle struct {
	pick       Preset
	hasOpinion bool
}

func (s stubOracle) Route(_ context.Context, _ string, _ []Preset) (Preset, bool) {
	return s.pick, s.hasOpinion
}

func TestRoute(t *testing.T) {
	ctx := context.Background()
	all := All()

	t.Run("nil oracle uses the heuristic", func(t *testing.T) {
		p, by := Route(ctx, nil, "build a new service", all)
		if p != Build || by != "heuristic" {
			t.Fatalf("Route = (%q,%q), want (build,heuristic)", p, by)
		}
	})

	t.Run("oracle with a confident, allowed pick wins", func(t *testing.T) {
		// Goal classifies as Run, but the oracle confidently picks Build.
		p, by := Route(ctx, stubOracle{pick: Build, hasOpinion: true}, "tweak a thing", all)
		if p != Build || by != "oracle" {
			t.Fatalf("Route = (%q,%q), want (build,oracle)", p, by)
		}
	})

	t.Run("oracle with no opinion falls back to heuristic", func(t *testing.T) {
		p, by := Route(ctx, stubOracle{pick: Swarm, hasOpinion: false}, "fix a bug", all)
		if p != Run || by != "heuristic" {
			t.Fatalf("Route = (%q,%q), want (run,heuristic)", p, by)
		}
	})

	t.Run("oracle pick outside allowed is ignored (fail-closed)", func(t *testing.T) {
		// Oracle wants Swarm but the caller only allows Run/Build → heuristic stands.
		p, by := Route(ctx, stubOracle{pick: Swarm, hasOpinion: true}, "build a new app", []Preset{Run, Build})
		if p != Build || by != "heuristic" {
			t.Fatalf("Route = (%q,%q), want (build,heuristic)", p, by)
		}
	})

	t.Run("oracle pick of an invalid preset is ignored", func(t *testing.T) {
		p, by := Route(ctx, stubOracle{pick: Preset("teleport"), hasOpinion: true}, "fix a bug", all)
		if p != Run || by != "heuristic" {
			t.Fatalf("Route = (%q,%q), want (run,heuristic)", p, by)
		}
	})

	t.Run("heuristic pick not in allowed falls back to first allowed", func(t *testing.T) {
		// Goal classifies as Swarm, but only Run is allowed → Route returns Run.
		p, by := Route(ctx, nil, "fix lint across all modules", []Preset{Run})
		if p != Run || by != "heuristic" {
			t.Fatalf("Route = (%q,%q), want (run,heuristic)", p, by)
		}
	})
}

func TestPresetValid(t *testing.T) {
	for _, p := range All() {
		if !p.Valid() {
			t.Errorf("All() contains invalid preset %q", p)
		}
	}
	if Preset("nope").Valid() {
		t.Error("unknown preset reported Valid")
	}
}
