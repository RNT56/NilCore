package tools

// patch.go — an all-or-nothing multi-file edit envelope (Codex apply_patch / Claude
// Code MultiEdit, generalized to many files). A coordinated cross-file change is one
// reversible unit instead of N independently-half-failing `edit` calls that can
// leave a broken intermediate tree. It is pure line-splicing confined to the
// worktree via the existing SafeJoin + atomic-write discipline — no AST, no
// execution, language-agnostic.
//
// Semantics: VALIDATE-ALL-THEN-WRITE-ALL. Every op is resolved and checked against
// current bytes first; if any op fails (missing context, ambiguous hunk, add over
// an existing file, confinement violation), NOTHING is written. Apply then snapshots
// each touched file's prior bytes in memory and restores them if a write syscall
// fails mid-batch, so a late failure can't leave a partial application.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"nilcore/internal/worktreefs"
)

// PatchTool applies a batch of file operations atomically.
type PatchTool struct{}

func (PatchTool) Name() string { return "patch" }
func (PatchTool) Description() string {
	return "Apply a batch of file ops atomically (all-or-nothing): add_file/update_file/delete_file, with " +
		"optional move. update_file carries hunks {context_before, removed, added, context_after} matched " +
		"against current lines. If any op fails to validate, nothing is written. Use for coordinated " +
		"cross-file edits. No execution."
}
func (PatchTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "ops": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "kind": {"type": "string", "enum": ["add_file", "update_file", "delete_file"]},
          "path": {"type": "string"},
          "move_to": {"type": "string"},
          "content": {"type": "string"},
          "hunks": {
            "type": "array",
            "items": {
              "type": "object",
              "properties": {
                "context_before": {"type": "array", "items": {"type": "string"}},
                "removed": {"type": "array", "items": {"type": "string"}},
                "added": {"type": "array", "items": {"type": "string"}},
                "context_after": {"type": "array", "items": {"type": "string"}}
              }
            }
          }
        },
        "required": ["kind", "path"]
      }
    }
  },
  "required": ["ops"]
}`)
}

type patchHunk struct {
	ContextBefore []string `json:"context_before"`
	Removed       []string `json:"removed"`
	Added         []string `json:"added"`
	ContextAfter  []string `json:"context_after"`
}

type patchOp struct {
	Kind    string      `json:"kind"`
	Path    string      `json:"path"`
	MoveTo  string      `json:"move_to"`
	Content string      `json:"content"`
	Hunks   []patchHunk `json:"hunks"`
}

// fileAction is one resolved effect: write data to abs, or delete abs. mode is the
// permission to create a NEW file with (a move preserves the source's mode so the
// executable bit is not lost); 0 means preserve-existing-else-0644.
type fileAction struct {
	abs    string
	rel    string
	delete bool
	data   []byte
	mode   os.FileMode
}

func (PatchTool) Run(_ context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Ops []patchOp `json:"ops"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	if len(in.Ops) == 0 {
		return "", fmt.Errorf("patch: no ops")
	}

	// Phase 1: validate every op, producing the concrete actions. No writes here.
	var actions []fileAction
	targets := map[string]bool{} // guard against two ops fighting over one path
	claim := func(abs string) error {
		if targets[abs] {
			return fmt.Errorf("two ops target the same path")
		}
		targets[abs] = true
		return nil
	}
	for i, op := range in.Ops {
		act, err := validateOp(workdir, op)
		if err != nil {
			return "", fmt.Errorf("patch op %d (%s %s): %w", i+1, op.Kind, op.Path, err)
		}
		for _, a := range act {
			if err := claim(a.abs); err != nil {
				return "", fmt.Errorf("patch op %d (%s %s): %w", i+1, op.Kind, op.Path, err)
			}
		}
		actions = append(actions, act...)
	}

	// Snapshot every touched file's prior state, for rollback on a mid-batch failure.
	type snap struct {
		abs     string
		existed bool
		data    []byte
	}
	snaps := make([]snap, 0, len(actions))
	for _, a := range actions {
		b, err := os.ReadFile(a.abs)
		if err == nil {
			snaps = append(snaps, snap{abs: a.abs, existed: true, data: b})
		} else if os.IsNotExist(err) {
			snaps = append(snaps, snap{abs: a.abs, existed: false})
		} else {
			return "", fmt.Errorf("patch: snapshot %s: %w", a.rel, err)
		}
	}
	// rollback restores every snapshot, COLLECTING any restore failures so a failed
	// rollback is surfaced (never silently reported as a clean revert).
	rollback := func() error {
		var failed []string
		for _, s := range snaps {
			var rerr error
			if s.existed {
				rerr = worktreefs.WriteConfined(s.abs, s.data, 0)
			} else if err := os.Remove(s.abs); err != nil && !os.IsNotExist(err) {
				rerr = err
			}
			if rerr != nil {
				failed = append(failed, s.abs)
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("rollback INCOMPLETE — could not restore %d file(s): %s", len(failed), strings.Join(failed, ", "))
		}
		return nil
	}
	failClosed := func(what string, cause error) error {
		if rbErr := rollback(); rbErr != nil {
			return fmt.Errorf("patch: %s failed (%v); %w — TREE MAY BE INCONSISTENT", what, cause, rbErr)
		}
		return fmt.Errorf("patch: %s failed, rolled back: %w", what, cause)
	}

	// Phase 2: apply. On the first failure, restore all snapshots and report.
	applied := 0
	for _, a := range actions {
		if a.delete {
			if err := os.Remove(a.abs); err != nil && !os.IsNotExist(err) {
				return "", failClosed("delete "+a.rel, err)
			}
		} else {
			if err := worktreefs.WriteConfined(a.abs, a.data, a.mode); err != nil {
				return "", failClosed("write "+a.rel, err)
			}
		}
		applied++
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "patch: applied %d op(s) across %d file action(s):\n", len(in.Ops), len(actions))
	for _, a := range actions {
		verb := "wrote"
		if a.delete {
			verb = "deleted"
		}
		fmt.Fprintf(&sb, "- %s %s\n", verb, a.rel)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// validateOp resolves and checks one op against the current tree, returning the
// concrete file actions it implies. It never writes.
func validateOp(workdir string, op patchOp) ([]fileAction, error) {
	abs, err := safePath(workdir, op.Path)
	if err != nil {
		return nil, err
	}
	switch op.Kind {
	case "add_file":
		if _, err := os.Stat(abs); err == nil {
			return nil, fmt.Errorf("file already exists")
		}
		return []fileAction{{abs: abs, rel: op.Path, data: []byte(op.Content)}}, nil

	case "delete_file":
		if _, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("file does not exist")
		}
		return []fileAction{{abs: abs, rel: op.Path, delete: true}}, nil

	case "update_file":
		info, serr := os.Stat(abs)
		if serr != nil {
			return nil, fmt.Errorf("stat: %w", serr)
		}
		cur, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
		var newContent string
		if len(op.Hunks) == 0 {
			if op.Content == "" {
				return nil, fmt.Errorf("update_file needs hunks or content")
			}
			newContent = op.Content
		} else {
			nc, herr := applyHunks(string(cur), op.Hunks)
			if herr != nil {
				return nil, herr
			}
			newContent = nc
		}
		if op.MoveTo != "" {
			dst, derr := safePath(workdir, op.MoveTo)
			if derr != nil {
				return nil, fmt.Errorf("move_to: %w", derr)
			}
			if _, err := os.Stat(dst); err == nil {
				return nil, fmt.Errorf("move_to target %q already exists", op.MoveTo)
			}
			// Preserve the source's mode on the new path so a moved executable script
			// does not silently lose its +x bit.
			return []fileAction{
				{abs: abs, rel: op.Path, delete: true},
				{abs: dst, rel: op.MoveTo, data: []byte(newContent), mode: info.Mode().Perm()},
			}, nil
		}
		return []fileAction{{abs: abs, rel: op.Path, data: []byte(newContent)}}, nil

	default:
		return nil, fmt.Errorf("unknown kind %q", op.Kind)
	}
}

// applyHunks applies each hunk to src in order. A hunk locates the contiguous run
// (context_before + removed + context_after) — which must occur EXACTLY once — and
// replaces it with (context_before + added + context_after). A hunk with empty
// `removed` is a pure insertion between the two context blocks. Any miss or
// ambiguity aborts (so the whole envelope writes nothing).
func applyHunks(src string, hunks []patchHunk) (string, error) {
	lines := strings.Split(src, "\n")
	for hi, h := range hunks {
		search := concatLines(h.ContextBefore, h.Removed, h.ContextAfter)
		replace := concatLines(h.ContextBefore, h.Added, h.ContextAfter)
		if len(search) == 0 {
			return "", fmt.Errorf("hunk %d is empty", hi+1)
		}
		idx, count := findRun(lines, search)
		if count == 0 {
			return "", fmt.Errorf("hunk %d: context not found", hi+1)
		}
		if count > 1 {
			return "", fmt.Errorf("hunk %d: context is ambiguous (%d matches) — add more context lines", hi+1, count)
		}
		next := make([]string, 0, len(lines)-len(search)+len(replace))
		next = append(next, lines[:idx]...)
		next = append(next, replace...)
		next = append(next, lines[idx+len(search):]...)
		lines = next
	}
	return strings.Join(lines, "\n"), nil
}

// concatLines joins the hunk's line groups into one slice.
func concatLines(groups ...[]string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// findRun returns the start index of the (unique) contiguous occurrence of `run`
// within `lines`, plus the total occurrence count.
func findRun(lines, run []string) (idx, count int) {
	idx = -1
	for i := 0; i+len(run) <= len(lines); i++ {
		match := true
		for j := range run {
			if lines[i+j] != run[j] {
				match = false
				break
			}
		}
		if match {
			if count == 0 {
				idx = i
			}
			count++
		}
	}
	return idx, count
}
