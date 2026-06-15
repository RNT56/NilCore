package session_test

import (
	"context"
	"path/filepath"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/session"
	"nilcore/internal/store"
	"nilcore/internal/summarize"
)

// *agent.Checkpoint is the production session.Store. Asserting it here (in an
// external test package) keeps the session package itself a leaf — it never
// imports agent/store — while still pinning the wiring contract at compile time.
var _ session.Store = (*agent.Checkpoint)(nil)

// TestCheckpointIsRealSessionStore drives a bounded WorkState through a real
// SQLite-backed Checkpoint: persist on a terminal drive, then a fresh Session for
// the same conversation ID restores it and continues via the restored driver.
// This is the end-to-end durability acceptance over the actual store write path
// (store.Task.Detail), not a fake.
func TestCheckpointIsRealSessionStore(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "conv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cp := agent.NewCheckpoint(st)
	ctx := context.Background()

	// First Session persists a bounded work-state via an explicit Checkpoint
	// (the same single-UpsertTask path a terminal drive uses). A nil event log is
	// safe (Append is nil-safe) — this test is about the store round-trip.
	s1 := session.New("conv-real", "local", "/repo", nil)
	s1.Store = cp
	s1.State = session.WorkState{
		Summary:     summarize.ContextSummary{Goal: "wire the API", Remaining: "add auth"},
		Active:      session.RouteSupervise,
		Branch:      "tip-real",
		LastOutcome: "build green",
	}
	if err := s1.Checkpoint(ctx); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// A conversation is recorded under the dedicated status so the backend-task
	// resume path never re-runs it as a coding task.
	got, err := st.GetTask(ctx, "conv-real")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != agent.ConversationStatus {
		t.Fatalf("conversation status = %q, want %q", got.Status, agent.ConversationStatus)
	}
	if got.Detail == "" {
		t.Fatal("conversation detail is empty after Checkpoint")
	}

	// A SECOND process restores the bounded state for the same ID.
	s2 := session.New("conv-real", "local", "/repo", nil)
	s2.Store = cp
	if restored := s2.Restore(ctx); !restored {
		t.Fatal("Restore reported restored=false after a real persist")
	}
	if s2.State.Summary.Goal != "wire the API" {
		t.Fatalf("restored Goal = %q", s2.State.Summary.Goal)
	}
	if s2.State.Active != session.RouteSupervise {
		t.Fatalf("restored Active = %v, want supervise", s2.State.Active)
	}
	if s2.State.Branch != "tip-real" {
		t.Fatalf("restored Branch = %q", s2.State.Branch)
	}
	if s2.State.LastOutcome != "build green" {
		t.Fatalf("restored LastOutcome = %q", s2.State.LastOutcome)
	}

	// A never-seen conversation restores clean (not-found, not an error).
	s3 := session.New("conv-never", "local", "/repo", nil)
	s3.Store = cp
	if restored := s3.Restore(ctx); restored {
		t.Fatal("Restore reported restored=true for a never-seen conversation")
	}
}
