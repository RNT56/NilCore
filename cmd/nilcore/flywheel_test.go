package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"nilcore/internal/agent"
)

// TestNewFlywheelLoopWiringHonorsCancel proves newFlywheelLoop builds a runnable,
// ctx-honoring loop without needing a model: a cancelled context stops it BEFORE any
// (expensive, model-driven) eval cycle runs. The loop's own logic is covered by
// internal/flywheel/loop; here we only assert the cmd-side wiring shape.
func TestNewFlywheelLoopWiringHonorsCancel(t *testing.T) {
	orch := &agent.Orchestrator{BaseRepo: t.TempDir()} // Execute is never reached
	// Deny-default gate: a shape assertion must never depend on a console/stdin.
	fw := newFlywheelLoop(orch, nil, filepath.Join(t.TempDir(), "e.jsonl"), 1, time.Minute,
		denyAllApprover{}.Approve)
	if fw == nil {
		t.Fatal("newFlywheelLoop must return a non-nil, runnable loop")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sum, err := fw.Run(ctx)
	if err == nil {
		t.Fatal("a cancelled context must stop the loop (before any eval cycle runs)")
	}
	if sum.Iterations != 0 {
		t.Fatalf("no cycle should run under a cancelled context, got %d", sum.Iterations)
	}
}
