package tools

// plan.go — a durable, host-side working-memory primitive (Claude Code TodoWrite /
// Codex task-tracker analog). The model maintains an ordered checklist that survives
// context compaction and turn boundaries, persisted as .nilcore/plan.json in the
// worktree. Its marginal worth over the model scribbling its own file is the three
// guarantees it enforces in code: a fixed schema, AT MOST ONE active step, and a
// stable render the operator surface (/status) can read back. Deterministic,
// worktree-confined, stdlib JSON only — no execution.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// planFile is the worktree-relative path the plan persists to.
const planFile = ".nilcore/plan.json"

// PlanStep is one checklist item. Status is one of pending|active|done|blocked.
type PlanStep struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// PlanTool reads and mutates the worktree plan.
type PlanTool struct{}

func (PlanTool) Name() string { return "plan" }
func (PlanTool) Description() string {
	return "Maintain a durable task checklist for this work (survives context compaction). op=get (show), " +
		"set (replace the whole step list), patch (update one step's status/note by id), reorder (by id list). " +
		"Steps have status pending|active|done|blocked; at most one is active. No execution."
}
func (PlanTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"op":{"type":"string","enum":["get","set","patch","reorder"]},` +
		`"steps":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"title":{"type":"string"},"status":{"type":"string","enum":["pending","active","done","blocked"]},"note":{"type":"string"}}}},` +
		`"id":{"type":"string"},"status":{"type":"string","enum":["pending","active","done","blocked"]},"note":{"type":"string"},` +
		`"order":{"type":"array","items":{"type":"string"}}},"required":["op"]}`)
}

var validStatus = map[string]bool{"pending": true, "active": true, "done": true, "blocked": true}

func (PlanTool) Run(_ context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Op     string     `json:"op"`
		Steps  []PlanStep `json:"steps"`
		ID     string     `json:"id"`
		Status string     `json:"status"`
		Note   string     `json:"note"`
		Order  []string   `json:"order"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}

	steps, err := loadPlan(workdir)
	if err != nil {
		return "", err
	}

	switch in.Op {
	case "get":
		return renderPlan(steps), nil

	case "set":
		next := make([]PlanStep, 0, len(in.Steps))
		for i, s := range in.Steps {
			if strings.TrimSpace(s.Title) == "" {
				return "", fmt.Errorf("step %d has no title", i+1)
			}
			if s.ID == "" {
				s.ID = fmt.Sprintf("s%d", i+1)
			}
			if s.Status == "" {
				s.Status = "pending"
			}
			if !validStatus[s.Status] {
				return "", fmt.Errorf("step %q has invalid status %q", s.ID, s.Status)
			}
			next = append(next, s)
		}
		steps = enforceSingleActive(next, "")

	case "patch":
		if in.ID == "" {
			return "", fmt.Errorf("patch requires id")
		}
		found := false
		for i := range steps {
			if steps[i].ID == in.ID {
				found = true
				if in.Status != "" {
					if !validStatus[in.Status] {
						return "", fmt.Errorf("invalid status %q", in.Status)
					}
					steps[i].Status = in.Status
				}
				if in.Note != "" {
					steps[i].Note = in.Note
				}
			}
		}
		if !found {
			return "", fmt.Errorf("no step with id %q", in.ID)
		}
		// If this patch set a step active, demote any other active step.
		if in.Status == "active" {
			steps = enforceSingleActive(steps, in.ID)
		}

	case "reorder":
		reordered, rerr := reorderSteps(steps, in.Order)
		if rerr != nil {
			return "", rerr
		}
		steps = reordered

	default:
		return "", fmt.Errorf("unknown op %q (want get|set|patch|reorder)", in.Op)
	}

	if in.Op != "get" {
		if err := savePlan(workdir, steps); err != nil {
			return "", err
		}
	}
	return renderPlan(steps), nil
}

// enforceSingleActive keeps at most one active step. keepID, if set and active,
// wins; otherwise the FIRST active step wins. The rest are demoted to pending.
func enforceSingleActive(steps []PlanStep, keepID string) []PlanStep {
	kept := false
	// First pass: if keepID is active, it is the one we keep.
	if keepID != "" {
		for _, s := range steps {
			if s.ID == keepID && s.Status == "active" {
				kept = true
				break
			}
		}
	}
	for i := range steps {
		if steps[i].Status != "active" {
			continue
		}
		if kept {
			if steps[i].ID != keepID {
				steps[i].Status = "pending"
			}
			continue
		}
		// No designated keeper yet: this first active becomes the keeper.
		kept = true
	}
	return steps
}

// reorderSteps returns steps in the order given by ids; ids must be a permutation
// of the existing step ids.
func reorderSteps(steps []PlanStep, order []string) ([]PlanStep, error) {
	if len(order) != len(steps) {
		return nil, fmt.Errorf("reorder needs all %d step id(s), got %d", len(steps), len(order))
	}
	byID := map[string]PlanStep{}
	for _, s := range steps {
		byID[s.ID] = s
	}
	out := make([]PlanStep, 0, len(steps))
	for _, id := range order {
		s, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("reorder references unknown id %q", id)
		}
		delete(byID, id)
		out = append(out, s)
	}
	if len(byID) != 0 {
		return nil, fmt.Errorf("reorder is missing some step ids")
	}
	return out, nil
}

// loadPlan reads the plan file; a missing file is an empty plan (not an error).
func loadPlan(workdir string) ([]PlanStep, error) {
	p, err := safePath(workdir, planFile)
	if err != nil {
		return nil, err
	}
	// O_NOFOLLOW read (readNoFollow): safePath checks the path, but a plain os.ReadFile
	// would follow a final-component symlink swapped in after that check and leak an
	// out-of-worktree file (I4 TOCTOU). ENOENT still classifies as os.IsNotExist below.
	b, err := readNoFollow(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var steps []PlanStep
	if len(b) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(b, &steps); err != nil {
		return nil, fmt.Errorf("plan: corrupt %s: %w", planFile, err)
	}
	return steps, nil
}

// savePlan writes the plan as indented JSON via the confined atomic-write path. It
// creates the .nilcore directory if needed.
func savePlan(workdir string, steps []PlanStep) error {
	p, err := safePath(workdir, planFile)
	if err != nil {
		return err
	}
	if dir, derr := safePath(workdir, ".nilcore"); derr == nil {
		_ = os.MkdirAll(dir, 0o755)
	}
	b, err := json.MarshalIndent(steps, "", "  ")
	if err != nil {
		return err
	}
	return writeNoFollow(workdir, p, b)
}

// renderPlan produces a compact, stable checklist view: a status glyph per step,
// its id, title, and any note, plus a done/total summary.
func renderPlan(steps []PlanStep) string {
	if len(steps) == 0 {
		return "plan: (empty) — use op=set to lay out the steps"
	}
	glyph := map[string]string{"pending": "[ ]", "active": "[~]", "done": "[x]", "blocked": "[!]"}
	done := 0
	var sb strings.Builder
	for _, s := range steps {
		g := glyph[s.Status]
		if g == "" {
			g = "[ ]"
		}
		if s.Status == "done" {
			done++
		}
		fmt.Fprintf(&sb, "%s %s %s", g, s.ID, s.Title)
		if s.Note != "" {
			fmt.Fprintf(&sb, "  — %s", s.Note)
		}
		sb.WriteByte('\n')
	}
	fmt.Fprintf(&sb, "(%d/%d done)", done, len(steps))
	return sb.String()
}
