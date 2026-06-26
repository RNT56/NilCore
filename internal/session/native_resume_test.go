package session

import (
	"context"
	"strings"
	"testing"

	"nilcore/internal/model"
	"nilcore/internal/summarize"
)

// TestNativeDriveSeedsRestoredSummary proves a native continuation after a process
// restart (History lost, bounded Summary restored) re-enters seeded with that
// Summary — so "continue" continues instead of silently restarting from the goal.
func TestNativeDriveSeedsRestoredSummary(t *testing.T) {
	var gotSeed []model.Message
	run := func(_ context.Context, nr NativeRun) (DriveOutcome, error) {
		gotSeed = nr.Seed
		return DriveOutcome{Summary: "ok", Verified: true}, nil
	}
	d := NewNativeDriver(run, nil, "conv")

	in := DriveInput{
		Route:   RouteContinue,
		Goal:    "keep going",
		History: []model.Message{userTurn("keep going")}, // only the new turn survived the restart
		State: WorkState{Summary: summarize.ContextSummary{
			Goal:      "build the X service",
			Remaining: "wire the Y handler",
		}},
	}
	if _, err := d.Drive(context.Background(), in); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if len(gotSeed) != 2 {
		t.Fatalf("seed length = %d, want 2 (restored-summary turn + the new goal)", len(gotSeed))
	}
	joined := strings.Join(text(gotSeed), "\n")
	if !strings.Contains(joined, "build the X service") || !strings.Contains(joined, "wire the Y handler") {
		t.Errorf("seed does not carry the restored summary:\n%s", joined)
	}
	if !strings.Contains(joined, "keep going") {
		t.Errorf("seed dropped the current goal turn:\n%s", joined)
	}
}

// TestNativeDriveNoSummaryInjectionNormally proves the resume-seed path is inert in
// normal operation: the seed equals the History when there is no restored summary
// (a fresh first turn) or when History has already grown in-process.
func TestNativeDriveNoSummaryInjectionNormally(t *testing.T) {
	cases := []struct {
		name string
		in   DriveInput
	}{
		{"fresh first turn (no summary)", DriveInput{
			Goal: "do it", History: []model.Message{userTurn("do it")},
		}},
		{"in-process continue (history already grown)", DriveInput{
			Goal:    "and then this",
			History: []model.Message{userTurn("do it"), userTurn("and then this")},
			State:   WorkState{Summary: summarize.ContextSummary{Goal: "do it"}},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotSeed []model.Message
			run := func(_ context.Context, nr NativeRun) (DriveOutcome, error) {
				gotSeed = nr.Seed
				return DriveOutcome{Verified: true}, nil
			}
			d := NewNativeDriver(run, nil, "c")
			if _, err := d.Drive(context.Background(), c.in); err != nil {
				t.Fatalf("Drive: %v", err)
			}
			if len(gotSeed) != len(c.in.History) {
				t.Errorf("seed length = %d, want %d (no injection)", len(gotSeed), len(c.in.History))
			}
		})
	}
}
