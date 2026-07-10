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

// TestClassifyWordBoundary pins the word-boundary matcher: common single-task phrasings
// whose keywords appear only as a SUBSTRING of a larger word must fall through to the cheap
// Run, never mis-route into the expensive build/swarm machines — while genuine build /
// swarm / breadth phrasings still route as intended.
func TestClassifyWordBoundary(t *testing.T) {
	// The four verified substring mis-routes. Each is an ordinary single task ⇒ Run.
	misroutes := []string{
		"build and test the parser",    // "build an" ⊂ "build and"
		"take a new approach to sync",  // "new app"  ⊂ "new approach"
		"rebuild all the tests",        // "build a"  ⊂ "rebuild all"
		"do the bulk of the work here", // bare "bulk" ⊂ "the bulk of"
	}
	for _, g := range misroutes {
		if got := Classify(g); got != Run {
			t.Errorf("Classify(%q) = %q, want Run (substring must not mis-route)", g, got)
		}
	}

	// Genuine phrasings still route where intended.
	genuine := []struct {
		goal string
		want Preset
	}{
		{"build a project from a spec", Build},
		{"build an app with a REST API", Build},
		{"scaffold a new service", Build},
		{"run a swarm across every service", Swarm},
		{"fix the flaky tests in parallel", Swarm},
		{"rename the fixtures in bulk", Swarm}, // "in bulk" is the genuine breadth phrasing
		{"decompose into subtasks", Run},       // decompose is opt-in, never auto-routed ⇒ safe default
		{"build  a   project", Build},          // irregular whitespace still matches (collapseWS)
	}
	for _, c := range genuine {
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

// TestDecomposePresetIsOptIn: the decompose preset is valid + reachable via an explicit
// pick, but Classify never auto-selects it (it is not in All()), so it stays opt-in.
func TestDecomposePresetIsOptIn(t *testing.T) {
	if !Decompose.Valid() {
		t.Fatal("Decompose must be a valid preset")
	}
	for _, p := range All() {
		if p == Decompose {
			t.Fatal("Decompose must NOT be in All() — it is opt-in, never auto-routed")
		}
	}
	// No heuristic goal should classify to decompose.
	for _, g := range []string{"build a new app and add tests", "audit every package", "fix a bug"} {
		if Classify(g) == Decompose {
			t.Fatalf("Classify(%q) auto-selected decompose; it must stay opt-in", g)
		}
	}
}
