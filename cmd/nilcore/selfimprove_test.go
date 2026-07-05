package main

// selfimprove_test.go covers the EXECUTION-time path screen shared by the
// interactive `nilcore propose-edit` flow (cmd/nilcore/selfimprove.go) and the
// autonomous flywheel loop (cmd/nilcore/flywheel.go): selfEditChangedPaths diffs a
// verified self-edit's kept branch against base HEAD so selfimprove.Flow.Changed can
// fail-close on any path the proposal never declared (including the verifier of
// record). Both wirings feed this ONE helper, so testing it hermetically covers the
// screen for both paths. The fixtures reuse the delivery-loop git helpers (a temp
// repo + a kept branch), which live in chat_delivery_test.go (package main).

import (
	"context"
	"testing"
)

// TestSelfEditChangedPathsFailsClosedOnEmptyBranch asserts the fail-closed contract:
// with no preserved branch (KeepBranch not honored) the helper returns an error, so
// selfimprove.Flow.Propose refuses to gate a run whose footprint it cannot prove.
func TestSelfEditChangedPathsFailsClosedOnEmptyBranch(t *testing.T) {
	repo := newDeliveryRepo(t)
	ctx := context.Background()

	for _, branch := range []string{"", "   "} {
		paths, err := selfEditChangedPaths(ctx, repo, branch)
		if err == nil {
			t.Errorf("selfEditChangedPaths(_, %q) must error (fail-closed), got paths=%v", branch, paths)
		}
		if paths != nil {
			t.Errorf("selfEditChangedPaths(_, %q) must return nil paths on error, got %v", branch, paths)
		}
	}
}

// TestSelfEditChangedPathsReportsExactDiff proves the helper returns exactly the
// repo-relative files the kept branch changed vs base HEAD — the evidence
// selfimprove.Flow screens against the proposal's declared Paths. A branch that
// touched a file the proposal never declared surfaces here (and Propose then
// fail-closes), which is the whole point of wiring Changed into the flywheel.
func TestSelfEditChangedPathsReportsExactDiff(t *testing.T) {
	repo := newDeliveryRepo(t) // base commit on main touches a.txt
	branch := "task/self-edit-1"
	addKeptBranch(t, repo, branch, "changed\n") // one commit editing a.txt

	paths, err := selfEditChangedPaths(context.Background(), repo, branch)
	if err != nil {
		t.Fatalf("selfEditChangedPaths: %v", err)
	}
	if len(paths) != 1 || paths[0] != "a.txt" {
		t.Fatalf("changed paths = %v, want exactly [a.txt]", paths)
	}
}

// TestSelfEditChangedPathsCatchesUndeclaredFile is the security case the screen
// exists for: a run that (steered by the free-text Goal) also writes an UNDECLARED
// file is reported here, so Propose can refuse to gate it. The helper reports every
// changed path; the Flow's scope+declared screen (internal/selfimprove) rejects the
// undeclared one. We assert the helper surfaces BOTH files so nothing is hidden.
func TestSelfEditChangedPathsCatchesUndeclaredFile(t *testing.T) {
	repo := newDeliveryRepo(t)
	branch := "task/self-edit-2"

	// Cut the branch, change the declared file AND an undeclared one in one commit.
	deliveryGit(t, repo, nil, "checkout", "-q", "-b", branch)
	writeDeliveryFile(t, repo, "a.txt", "declared\n")
	writeDeliveryFile(t, repo, "sneaky.txt", "undeclared\n")
	deliveryGit(t, repo, nil, "add", "-A")
	deliveryGit(t, repo, nil, "-c", "user.email=t@nilcore.local", "-c", "user.name=t",
		"commit", "-q", "-m", "edit + sneak")
	deliveryGit(t, repo, nil, "checkout", "-q", "main")

	paths, err := selfEditChangedPaths(context.Background(), repo, branch)
	if err != nil {
		t.Fatalf("selfEditChangedPaths: %v", err)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p] = true
	}
	if !seen["a.txt"] || !seen["sneaky.txt"] {
		t.Fatalf("changed paths = %v, want both a.txt and the undeclared sneaky.txt surfaced", paths)
	}
}
