// Package planner decomposes a goal into an explicit, inspectable task tree
// (P3-T01). It is adaptive — invoked only for complex tasks — and contract-first
// (principle #6): every task states its acceptance criteria (ideally a failing
// test) before any code is written. The plan is produced by the strong advisor
// model and is schema-validated JSON, so it can be reviewed or edited.
package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"nilcore/internal/model"
)

// PlanTask is one node of the plan.
type PlanTask struct {
	ID         string   `json:"id"`
	Goal       string   `json:"goal"`
	DependsOn  []string `json:"depends_on"`
	Acceptance string   `json:"acceptance"` // how "done" is defined, before coding
}

// Tree is a decomposed goal.
type Tree struct {
	Goal  string     `json:"goal"`
	Tasks []PlanTask `json:"tasks"`
}

// Validate checks the plan is well-formed: tasks exist, each has an id/goal and
// acceptance criteria (contract-first), and dependencies reference real tasks.
func (t Tree) Validate() error {
	if len(t.Tasks) == 0 {
		return errors.New("plan has no tasks")
	}
	ids := make(map[string]bool, len(t.Tasks))
	for _, task := range t.Tasks {
		if task.ID == "" || task.Goal == "" {
			return errors.New("plan task missing id or goal")
		}
		if task.Acceptance == "" {
			return fmt.Errorf("task %s has no acceptance criteria (contract-first)", task.ID)
		}
		if ids[task.ID] {
			return fmt.Errorf("duplicate task id %s", task.ID)
		}
		ids[task.ID] = true
	}
	for _, task := range t.Tasks {
		for _, d := range task.DependsOn {
			if !ids[d] {
				return fmt.Errorf("task %s depends on unknown task %s", task.ID, d)
			}
		}
	}
	return nil
}

const sysPrompt = `Decompose the goal into a minimal task tree. Respond with ONLY JSON:
{"goal": string, "tasks": [{"id": string, "goal": string, "depends_on": [string], "acceptance": string}]}.
Every task MUST state "acceptance" — how "done" is verified, ideally a failing test — before any code.
Keep the tree as small as the goal honestly requires.`

// Plan asks the model to decompose goal into a validated task tree.
func Plan(ctx context.Context, m model.Provider, goal string) (Tree, error) {
	msgs := []model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "Goal:\n" + goal}}}}
	resp, err := m.Complete(ctx, sysPrompt, msgs, nil, 2048)
	if err != nil {
		return Tree{}, fmt.Errorf("plan: %w", err)
	}
	tree, err := parse(firstText(resp.Content))
	if err != nil {
		return Tree{}, err
	}
	if tree.Goal == "" {
		tree.Goal = goal
	}
	if err := tree.Validate(); err != nil {
		return Tree{}, fmt.Errorf("invalid plan: %w", err)
	}
	return tree, nil
}

func firstText(blocks []model.Block) string {
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			return b.Text
		}
	}
	return ""
}

func parse(s string) (Tree, error) {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return Tree{}, errors.New("no JSON object in plan output")
	}
	var t Tree
	if err := json.Unmarshal([]byte(s[start:end+1]), &t); err != nil {
		return Tree{}, fmt.Errorf("parse plan: %w", err)
	}
	return t, nil
}
