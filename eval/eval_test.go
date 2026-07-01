package eval

import (
	"context"
	"strings"
	"testing"
)

func TestRunReport(t *testing.T) {
	cases := []Case{{Name: "fix-bug", Goal: "x"}, {Name: "add-feature", Goal: "y"}}
	run := func(_ context.Context, c Case) (bool, float64) {
		return c.Name == "fix-bug", 1.5 // one passes, each costs 1.5
	}
	rep := Run(context.Background(), cases, "native:claude-sonnet-4-6", run)

	if len(rep.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(rep.Results))
	}
	if rep.PassRate != 0.5 {
		t.Errorf("pass rate = %v, want 0.5", rep.PassRate)
	}
	if rep.TotalCost != 3.0 {
		t.Errorf("total cost = %v, want 3.0", rep.TotalCost)
	}
	if rep.Config != "native:claude-sonnet-4-6" {
		t.Errorf("config = %q", rep.Config)
	}
	for _, r := range rep.Results {
		if r.Latency < 0 {
			t.Errorf("latency not measured for %s", r.Case)
		}
	}

	b, err := rep.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"pass_rate"`) || !strings.Contains(string(b), `"results"`) {
		t.Errorf("report JSON missing fields: %s", b)
	}
}

func TestRunEmpty(t *testing.T) {
	rep := Run(context.Background(), nil, "x", func(context.Context, Case) (bool, float64) { return true, 0 })
	if rep.PassRate != 0 || len(rep.Results) != 0 {
		t.Errorf("empty suite report = %+v", rep)
	}
}

// A cancelled suite must stop between cases, not grind through (and score as failed)
// every remaining case. PassRate then reflects only the cases actually attempted.
func TestRunCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cases := []Case{{Name: "a", Goal: "x"}, {Name: "b", Goal: "y"}, {Name: "c", Goal: "z"}}

	var ran int
	run := func(_ context.Context, _ Case) (bool, float64) {
		ran++
		cancel() // cancel right after the first case runs
		return true, 1.0
	}
	rep := Run(ctx, cases, "cfg", run)

	if ran != 1 {
		t.Fatalf("run invoked %d times, want 1 (loop should stop after cancellation)", ran)
	}
	if len(rep.Results) != 1 {
		t.Fatalf("results = %d, want 1 (fewer than %d cases)", len(rep.Results), len(cases))
	}
	// PassRate scores only the attempted case, not the whole (mostly-unrun) suite.
	if rep.PassRate != 1.0 {
		t.Errorf("pass rate = %v, want 1.0 over the single attempted case", rep.PassRate)
	}
	if rep.TotalCost != 1.0 {
		t.Errorf("total cost = %v, want 1.0", rep.TotalCost)
	}
}
