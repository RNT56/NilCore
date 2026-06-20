// Package requeue is the stdlib-only, leaf engine for Pillar 4 — granular
// requeue. It makes a failed run addressable at the granularity of ONE claim:
// instead of "the run failed, try again", it derives a worklist of exactly the
// red cells ("company-041 revenue mismatch / source 404 / margin missing") so a
// swarm can fix the broken claim, not the world.
//
// WHY a leaf. requeue derives its worklist from the verifier-SET claim statuses
// (artifact.Status), never from a model self-report — so it imports only
// internal/artifact (+ stdlib) and nothing from the orchestrator (spawn/super/
// store). That keeps it a pure, hermetically testable transform: artifacts in,
// a Worklist out. The actual re-dispatch reuses the existing DAG + ContinueFrom
// verbatim (Pillar 4 invents no loop); this package only computes WHAT to redo.
//
// Trust boundary (I7): a Unit is built from harness-trusted Status/Detail plus
// the model-authored ClaimID/Field/ArtifactID, which are carried as DATA (a
// requeue key + label), never interpreted as instructions. Status routes the
// fix: fail ⇒ re-derive the value, stale ⇒ re-fetch the source, unverifiable ⇒
// fix the source or the verifier binding.
package requeue

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"nilcore/internal/artifact"
)

// artifactsRel is the fixed, out-of-band carrier directory every Phase 11 pillar
// agrees on: artifacts live at <root>/.nilcore/artifacts/<id>.json. The segments
// are harness constants (no model-authored path component), so the directory walk
// needs no symlink confinement — only the per-file parse trusts the worktreefs
// discipline already baked into the artifact package.
const artifactsRel = ".nilcore/artifacts"

// Unit is one addressable failure: a single non-pass claim in a single artifact.
// It is the atom the requeue planner groups into focused re-dispatch subtasks and
// the retry Ledger counts attempts against (keyed ArtifactID/ClaimID). Every field
// is plain data: ArtifactID/ClaimID/Field/OwnerSubagent are labels, Status/Detail
// are the verifier's trusted verdict, Attempt is stamped from the Ledger.
type Unit struct {
	ArtifactID    string          `json:"artifact_id"`
	ClaimID       string          `json:"claim_id"`
	Field         string          `json:"field"`
	Status        artifact.Status `json:"status"`
	Detail        string          `json:"detail,omitempty"`
	OwnerSubagent string          `json:"owner_subagent,omitempty"`
	Attempt       int             `json:"attempt"`
}

// Worklist is the full set of currently-failing Units across every persisted
// artifact in a worktree. An empty Worklist (no Units) means nothing is red — the
// convergence condition the requeue loop drives toward.
type Worklist struct {
	Units []Unit `json:"units"`
}

// Ledger is the per-Unit retry budget. The complete budget semantics
// (MaxAttempts, Bump, Exhausted, Marshal/UnmarshalLedger) are owned by P11-T20;
// this file defines only the minimum Scan needs — the attempt counter keyed
// ArtifactID/ClaimID — so Scan can stamp Unit.Attempt from prior history. A nil
// Ledger means "no history": every Unit is stamped Attempt 0.
type Ledger struct {
	// MaxAttempts bounds requeue; MaxAttempts==0 disables requeue entirely (the
	// default-off path). Defined here so the struct shape is stable across T19/T20.
	MaxAttempts int `json:"max_attempts"`
	// Attempts is the per-Unit counter keyed by key(Unit). nil is a valid empty
	// ledger (resumes disabled).
	Attempts map[string]int `json:"attempts,omitempty"`
}

// key is the stable per-Unit identity used by both the Ledger and the planner:
// the artifact id and claim id joined, so two artifacts sharing a claim id stay
// distinct and one claim's retries never bleed into another's.
func key(u Unit) string { return u.ArtifactID + "/" + u.ClaimID }

// attemptFor returns the recorded attempt count for a Unit, or 0 when the Ledger
// (or its map) has no entry. It is nil-safe so Scan can be called without a
// Ledger when requeue history is irrelevant (e.g. an initial worklist).
func (l *Ledger) attemptFor(u Unit) int {
	if l == nil || l.Attempts == nil {
		return 0
	}
	return l.Attempts[key(u)]
}

// Scan walks <root>/.nilcore/artifacts/*.json and derives a Worklist holding
// exactly one Unit per non-StatusPass claim — a pass claim contributes no Unit, so
// an all-pass (or artifact-free) worktree yields an empty Worklist. Each Unit's
// Attempt is stamped from led (0 when led is nil or has no record).
//
// Fail-closed reading: a missing artifacts directory is NOT an error (a run that
// produced no artifacts simply has nothing to requeue) and yields an empty
// Worklist; but a present-yet-corrupt artifact file is a hard error, never a
// silent empty — a parse failure must not be mistaken for "all green".
//
// Determinism: artifacts are visited in sorted filename order and claims in their
// declared order, so the resulting Worklist is stable across runs (meaningful for
// golden tests and reproducible planning).
func Scan(root string, led *Ledger) (Worklist, error) {
	dir := filepath.Join(root, filepath.FromSlash(artifactsRel))
	ents, err := os.ReadDir(dir)
	if err != nil {
		// A worktree that never wrote an artifact has no carrier dir — that is the
		// empty, no-error case, distinct from a corrupt file below.
		if errors.Is(err, fs.ErrNotExist) {
			return Worklist{}, nil
		}
		return Worklist{}, fmt.Errorf("requeue: read artifacts dir: %w", err)
	}

	// Sort entry names so the walk order — and thus the Worklist — is deterministic.
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var wl Worklist
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return Worklist{}, fmt.Errorf("requeue: read artifact %q: %w", name, err)
		}
		art, err := artifact.Unmarshal(data)
		if err != nil {
			// Corrupt JSON is surfaced, never swallowed: treating an unparseable
			// artifact as "no failed claims" would falsely report convergence.
			return Worklist{}, fmt.Errorf("requeue: parse artifact %q: %w", name, err)
		}
		for i := range art.Claims {
			c := art.Claims[i]
			if c.Evidence.Status == artifact.StatusPass {
				continue
			}
			u := Unit{
				ArtifactID: art.ID,
				ClaimID:    c.ID,
				Field:      c.Field,
				Status:     c.Evidence.Status,
				Detail:     c.Evidence.Detail,
			}
			u.Attempt = led.attemptFor(u)
			wl.Units = append(wl.Units, u)
		}
	}
	return wl, nil
}
