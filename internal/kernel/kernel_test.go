package kernel

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func flatRunner(tag string) RunFunc {
	return func(_ context.Context, n Node) (Outcome, error) {
		return Outcome{Backend: tag, Summary: "flat:" + n.ID, Verified: true}, nil
	}
}

// TestRunDispatchesByGranularity: Flat vs Decompose is chosen by the policy, gated by the
// presence of a Decompose runner.
func TestRunDispatchesByGranularity(t *testing.T) {
	ctx := context.Background()

	// Flat-only envelope (no Decompose): always runs Flat even with AlwaysDecompose.
	flatOnly := Envelope{Name: "run", Flat: flatRunner("flat"), Granularity: AlwaysDecompose}
	if out, err := Run(ctx, flatOnly, Node{ID: "a"}); err != nil || out.Summary != "flat:a" {
		t.Fatalf("flat-only must run flat, got %+v err=%v", out, err)
	}

	// Envelope with a Decompose runner + AlwaysDecompose ⇒ decompose.
	decomposed := false
	env := Envelope{
		Name: "build",
		Flat: flatRunner("flat"),
		Decompose: func(_ context.Context, n Node) (Outcome, error) {
			decomposed = true
			return Outcome{Summary: "dec:" + n.ID, Verified: true}, nil
		},
		Granularity: AlwaysDecompose,
		MaxDepth:    2,
	}
	if out, err := Run(ctx, env, Node{ID: "b"}); err != nil || out.Summary != "dec:b" || !decomposed {
		t.Fatalf("AlwaysDecompose must run decompose, got %+v err=%v", out, err)
	}

	// AlwaysFlat with a Decompose runner present ⇒ still flat.
	env.Granularity = AlwaysFlat
	if out, _ := Run(ctx, env, Node{ID: "c"}); out.Summary != "flat:c" {
		t.Fatalf("AlwaysFlat must run flat, got %+v", out)
	}
}

// TestNoFlatIsError: every envelope needs a base-case runner.
func TestNoFlatIsError(t *testing.T) {
	if _, err := Run(context.Background(), Envelope{Name: "x"}, Node{ID: "a"}); !errors.Is(err, ErrNoFlat) {
		t.Fatalf("missing Flat runner must return ErrNoFlat, got %v", err)
	}
}

// TestDepthBoundForcesFlat: at MaxDepth a node runs Flat regardless of Granularity, so
// recursion can never run away.
func TestDepthBoundForcesFlat(t *testing.T) {
	env := Envelope{
		Name: "build",
		Flat: flatRunner("flat"),
		Decompose: func(context.Context, Node) (Outcome, error) {
			t.Fatal("decompose must not run at the depth bound")
			return Outcome{}, nil
		},
		Granularity: AlwaysDecompose,
		MaxDepth:    1,
	}
	// Depth 1 == MaxDepth ⇒ Flat (the deepest level's children are single tasks).
	if out, err := Run(context.Background(), env, Node{ID: "deep", Depth: 1}); err != nil || out.Summary != "flat:deep" {
		t.Fatalf("at MaxDepth must run flat, got %+v err=%v", out, err)
	}
}

// TestRecursiveFansOutAndIntegrates: the kernel-driven recursive engine plans, runs each
// child through Run (recursing — a child may itself decompose), then integrates, and the
// integrator is the SOLE judge of the tip (I2).
func TestRecursiveFansOutAndIntegrates(t *testing.T) {
	ctx := context.Background()

	// A 2-level tree: root decomposes into 2 children; child "c0" itself decomposes into 2
	// grandchildren; "c1" runs flat. MaxDepth=2 admits the second level.
	var ranFlat []string
	plan := func(_ context.Context, n Node) ([]Node, error) {
		switch n.ID {
		case "root":
			return []Node{{ID: "c0", Goal: "decompose-me"}, {ID: "c1"}}, nil
		case "c0":
			return []Node{{ID: "g0"}, {ID: "g1"}}, nil
		}
		return nil, nil
	}
	integrate := func(_ context.Context, n Node, children []Outcome) (Outcome, error) {
		// The tip is verified only if EVERY child verified AND the (here, stubbed) tip
		// re-verify passes — modelling the I2 obligation that green children ≠ done.
		allChildrenGreen := true
		for _, c := range children {
			if !c.Verified {
				allChildrenGreen = false
			}
		}
		return Outcome{Summary: fmt.Sprintf("integrated:%s(%d)", n.ID, len(children)), Verified: allChildrenGreen}, nil
	}

	env := Envelope{
		Name: "recursive",
		Flat: func(_ context.Context, n Node) (Outcome, error) {
			ranFlat = append(ranFlat, n.ID)
			return Outcome{Summary: "flat:" + n.ID, Verified: true}, nil
		},
		Granularity: GranularityFunc(func(_ context.Context, n Node, _ Envelope) Branch {
			if n.ID == "root" || n.ID == "c0" {
				return Decompose
			}
			return Flat
		}),
		MaxDepth: 3,
	}
	env.Decompose = Recursive(&env, plan, integrate)

	out, err := Run(ctx, env, Node{ID: "root"})
	if err != nil {
		t.Fatalf("recursive run: %v", err)
	}
	// root integrated 2 children; c0 integrated 2 grandchildren; the flats were the
	// grandchildren (g0,g1) + c1.
	if out.Summary != "integrated:root(2)" || !out.Verified {
		t.Fatalf("root outcome = %+v, want integrated:root(2) verified", out)
	}
	wantFlat := map[string]bool{"g0": true, "g1": true, "c1": true}
	if len(ranFlat) != 3 {
		t.Fatalf("expected 3 flat leaf runs (g0,g1,c1), got %v", ranFlat)
	}
	for _, id := range ranFlat {
		if !wantFlat[id] {
			t.Fatalf("unexpected flat run %q (only the leaves should run flat); got %v", id, ranFlat)
		}
	}
}

// TestRecursiveTipReVerifyOverridesGreenChildren: even when every child verified, the
// integrator (the tip re-verify) is the authority — a red tip re-verify makes the parent
// NOT done (the review's I2 fix: green children can integrate red).
func TestRecursiveTipReVerifyOverridesGreenChildren(t *testing.T) {
	plan := func(context.Context, Node) ([]Node, error) { return []Node{{ID: "c0"}, {ID: "c1"}}, nil }
	// The integrated tip re-verify FAILS even though both children are green.
	integrate := func(_ context.Context, n Node, children []Outcome) (Outcome, error) {
		for _, c := range children {
			if !c.Verified {
				t.Fatal("test setup: children should be green")
			}
		}
		return Outcome{Summary: "tip-red", Verified: false}, nil // tip re-verify says NO
	}
	env := Envelope{
		Name:        "build",
		Flat:        flatRunner("flat"), // the children run flat (depth 1 == MaxDepth) and verify green
		Granularity: AlwaysDecompose,
		MaxDepth:    1, // only the root decomposes; its children are green flat leaves
	}
	env.Decompose = Recursive(&env, plan, integrate)

	out, err := Run(context.Background(), env, Node{ID: "root"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Verified {
		t.Fatal("a red integrated tip must make the parent NOT verified, even with all-green children (I2)")
	}
}

// TestRecursiveNeedsPlanAndIntegrate: a misbuilt recursive decompose fails loud.
func TestRecursiveNeedsPlanAndIntegrate(t *testing.T) {
	env := Envelope{Name: "x", Flat: flatRunner("f"), Granularity: AlwaysDecompose, MaxDepth: 2}
	env.Decompose = Recursive(&env, nil, nil)
	if _, err := Run(context.Background(), env, Node{ID: "a"}); err == nil {
		t.Fatal("recursive decompose with nil Plan/Integrate must error")
	}
}

// TestRunHonorsContextCancel: a cancelled context stops before running.
func TestRunHonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, Envelope{Name: "run", Flat: flatRunner("f")}, Node{ID: "a"}); err == nil {
		t.Fatal("a cancelled context must stop Run")
	}
}
