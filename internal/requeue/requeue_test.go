package requeue

import (
	"os"
	"path/filepath"
	"testing"

	"nilcore/internal/artifact"
)

// writeArtifact persists an artifact into the fixed carrier dir of a fresh temp
// worktree root, exercising the same on-disk shape Scan reads. It returns the root.
func writeArtifact(t *testing.T, root string, a *artifact.Artifact) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(artifactsRel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	data, err := artifact.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, a.ID+".json"), data, 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
}

func claim(id, field string, st artifact.Status) artifact.Claim {
	return artifact.Claim{
		ID:    id,
		Field: field,
		Evidence: artifact.Evidence{
			Value:  "v-" + id,
			Status: st,
			Detail: "detail-" + id,
		},
	}
}

func TestScan(t *testing.T) {
	t.Run("one unit per non-pass claim, pass claim skipped", func(t *testing.T) {
		root := t.TempDir()
		writeArtifact(t, root, &artifact.Artifact{
			ID:   "co-041",
			Kind: artifact.KindMatrix,
			Claims: []artifact.Claim{
				claim("c-pass", "revenue", artifact.StatusPass),
				claim("c-fail", "margin", artifact.StatusFail),
				claim("c-stale", "price", artifact.StatusStale),
				claim("c-unver", "rating", artifact.StatusUnverifiable),
			},
		})

		wl, err := Scan(root, nil)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		// Exactly 3 Units (the pass claim contributes none).
		if len(wl.Units) != 3 {
			t.Fatalf("got %d units, want 3: %+v", len(wl.Units), wl.Units)
		}
		want := []struct {
			claimID string
			field   string
			status  artifact.Status
		}{
			{"c-fail", "margin", artifact.StatusFail},
			{"c-stale", "price", artifact.StatusStale},
			{"c-unver", "rating", artifact.StatusUnverifiable},
		}
		for i, w := range want {
			u := wl.Units[i]
			if u.ArtifactID != "co-041" {
				t.Errorf("unit %d: ArtifactID=%q, want co-041", i, u.ArtifactID)
			}
			if u.ClaimID != w.claimID || u.Field != w.field || u.Status != w.status {
				t.Errorf("unit %d: got {%q,%q,%q}, want {%q,%q,%q}",
					i, u.ClaimID, u.Field, u.Status, w.claimID, w.field, w.status)
			}
			if u.Detail != "detail-"+w.claimID {
				t.Errorf("unit %d: Detail=%q, want carried verifier detail", i, u.Detail)
			}
			if u.Attempt != 0 {
				t.Errorf("unit %d: Attempt=%d, want 0 (nil ledger)", i, u.Attempt)
			}
		}
	})

	t.Run("attempt stamped from ledger", func(t *testing.T) {
		root := t.TempDir()
		writeArtifact(t, root, &artifact.Artifact{
			ID:     "co-007",
			Kind:   artifact.KindReport,
			Claims: []artifact.Claim{claim("c-x", "f", artifact.StatusFail)},
		})
		led := &Ledger{MaxAttempts: 3, Attempts: map[string]int{"co-007/c-x": 2}}
		wl, err := Scan(root, led)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(wl.Units) != 1 {
			t.Fatalf("got %d units, want 1", len(wl.Units))
		}
		if wl.Units[0].Attempt != 2 {
			t.Errorf("Attempt=%d, want 2 (from ledger)", wl.Units[0].Attempt)
		}
	})

	t.Run("ledger without matching key stamps 0", func(t *testing.T) {
		root := t.TempDir()
		writeArtifact(t, root, &artifact.Artifact{
			ID:     "co-009",
			Kind:   artifact.KindReport,
			Claims: []artifact.Claim{claim("c-y", "f", artifact.StatusFail)},
		})
		led := &Ledger{MaxAttempts: 3, Attempts: map[string]int{"other/zzz": 5}}
		wl, err := Scan(root, led)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if wl.Units[0].Attempt != 0 {
			t.Errorf("Attempt=%d, want 0 (no matching ledger key)", wl.Units[0].Attempt)
		}
	})

	t.Run("all-pass artifact yields empty worklist", func(t *testing.T) {
		root := t.TempDir()
		writeArtifact(t, root, &artifact.Artifact{
			ID:   "co-100",
			Kind: artifact.KindReport,
			Claims: []artifact.Claim{
				claim("c-1", "a", artifact.StatusPass),
				claim("c-2", "b", artifact.StatusPass),
			},
		})
		wl, err := Scan(root, nil)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(wl.Units) != 0 {
			t.Fatalf("got %d units, want 0 (all pass)", len(wl.Units))
		}
	})

	t.Run("missing artifacts dir is empty no error", func(t *testing.T) {
		root := t.TempDir() // no .nilcore/artifacts created
		wl, err := Scan(root, nil)
		if err != nil {
			t.Fatalf("Scan: unexpected error for missing dir: %v", err)
		}
		if len(wl.Units) != 0 {
			t.Fatalf("got %d units, want 0", len(wl.Units))
		}
	})

	t.Run("zero artifacts (empty dir) yields empty worklist", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(artifactsRel)), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		wl, err := Scan(root, nil)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(wl.Units) != 0 {
			t.Fatalf("got %d units, want 0", len(wl.Units))
		}
	})

	t.Run("corrupt json is an error not silent empty", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, filepath.FromSlash(artifactsRel))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := Scan(root, nil)
		if err == nil {
			t.Fatal("Scan: want error on corrupt JSON, got nil")
		}
	})

	t.Run("multiple artifacts visited in sorted order", func(t *testing.T) {
		root := t.TempDir()
		writeArtifact(t, root, &artifact.Artifact{
			ID:     "zeta",
			Kind:   artifact.KindReport,
			Claims: []artifact.Claim{claim("z1", "f", artifact.StatusFail)},
		})
		writeArtifact(t, root, &artifact.Artifact{
			ID:     "alpha",
			Kind:   artifact.KindReport,
			Claims: []artifact.Claim{claim("a1", "f", artifact.StatusFail)},
		})
		wl, err := Scan(root, nil)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(wl.Units) != 2 {
			t.Fatalf("got %d units, want 2", len(wl.Units))
		}
		// alpha.json sorts before zeta.json.
		if wl.Units[0].ArtifactID != "alpha" || wl.Units[1].ArtifactID != "zeta" {
			t.Errorf("order = [%q,%q], want [alpha,zeta]",
				wl.Units[0].ArtifactID, wl.Units[1].ArtifactID)
		}
	})

	t.Run("non-json files ignored", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, filepath.FromSlash(artifactsRel))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("ignore me"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		writeArtifact(t, root, &artifact.Artifact{
			ID:     "real",
			Kind:   artifact.KindReport,
			Claims: []artifact.Claim{claim("r1", "f", artifact.StatusFail)},
		})
		wl, err := Scan(root, nil)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(wl.Units) != 1 {
			t.Fatalf("got %d units, want 1 (non-json ignored)", len(wl.Units))
		}
	})
}

// TestLedgerAttemptFor guards the nil-safe attempt lookup Scan relies on; the full
// Ledger budget surface (Bump/Exhausted/Marshal) is P11-T20's.
func TestLedgerAttemptFor(t *testing.T) {
	u := Unit{ArtifactID: "a", ClaimID: "c"}
	var nilLed *Ledger
	if got := nilLed.attemptFor(u); got != 0 {
		t.Errorf("nil ledger attemptFor=%d, want 0", got)
	}
	empty := &Ledger{}
	if got := empty.attemptFor(u); got != 0 {
		t.Errorf("empty ledger attemptFor=%d, want 0", got)
	}
	led := &Ledger{Attempts: map[string]int{"a/c": 4}}
	if got := led.attemptFor(u); got != 4 {
		t.Errorf("attemptFor=%d, want 4", got)
	}
	if key(u) != "a/c" {
		t.Errorf("key=%q, want a/c", key(u))
	}
}
