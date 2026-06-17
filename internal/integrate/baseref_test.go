package integrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBaseRefBuildsOnPreservedTip is the durable-resume integration property: with
// BaseRef set to a preserved tip (a commit NOT on main), a re-integration starts from
// THAT tip — the already-merged work is present — and folds the remaining branch on top
// of it, instead of rebuilding from base HEAD (which would orphan the merged work).
func TestBaseRefBuildsOnPreservedTip(t *testing.T) {
	repo := baseRepo(t)

	// A "preserved tip": main + t1's merged work, captured as a SHA the way a prior
	// run's snapshot would have pinned it. We model it as a commit on a side branch.
	branchFrom(t, repo, "task/t1", map[string]string{"t1.txt": "from t1\n"})
	hgit(t, repo, "checkout", "-q", "-b", "preserved", "main")
	hgit(t, repo, "merge", "--no-ff", "-q", "-m", "integrate t1", "task/t1")
	tip := strings.TrimSpace(hgit(t, repo, "rev-parse", "preserved"))
	hgit(t, repo, "checkout", "-q", "main") // base advances no further; the tip is off main

	// A remaining branch t2 (cut from main — its dep branch t1 is "gone" on resume).
	branchFrom(t, repo, "task/t2", map[string]string{"t2.txt": "from t2\n"})

	log, _ := testLog(t)
	it := &Integrator{
		BaseRepo: repo,
		BaseRef:  tip, // resume: build on the preserved tip, not HEAD
		NewEnv:   newEnvFor("README", func(string) bool { return true }),
		Log:      log,
	}
	wt, results, err := it.Integrate(context.Background(), []MergeItem{{ID: "t2", Branch: "task/t2"}})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	if len(results) != 1 || !results[0].Merged || !results[0].Verified {
		t.Fatalf("t2 should merge green onto the preserved tip: %+v", results)
	}
	// t1's merged work (from the preserved tip) is PRESENT — no work lost.
	if _, err := os.Stat(filepath.Join(wt.Path(), "t1.txt")); err != nil {
		t.Errorf("preserved tip's merged work (t1.txt) missing from re-integration: %v", err)
	}
	// t2's new work is folded on top.
	if _, err := os.Stat(filepath.Join(wt.Path(), "t2.txt")); err != nil {
		t.Errorf("remaining work (t2.txt) not integrated: %v", err)
	}
	// The pre-merge tip was the preserved tip, not base HEAD — proving BaseRef was honored.
	if results[0].PreSHA != tip {
		t.Errorf("integration started from %q, want the preserved tip %q", results[0].PreSHA, tip)
	}

	// Sanity: empty BaseRef is byte-identical to HEAD (the default path).
	it2 := &Integrator{BaseRepo: repo, NewEnv: newEnvFor("README", func(string) bool { return true }), Log: log}
	wt2, _, err := it2.Integrate(context.Background(), nil)
	if err != nil {
		t.Fatalf("default-BaseRef integrate: %v", err)
	}
	defer func() { _ = wt2.Cleanup() }()
	if h, _ := wt2.Head(context.Background()); h != baseHead(t, repo) {
		t.Errorf("default BaseRef did not start from HEAD: %q != %q", h, baseHead(t, repo))
	}
}
