// Package summarize produces a compact ContextSummary that bounds context at
// every level without losing intent (P3-T06): the spawner seeds fresh subworkers
// with one, results fold back as summaries rather than full transcripts, and the
// native loop can self-handoff through the same path when its window fills.
package summarize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nilcore/internal/model"
)

// ContextSummary is the minimal carry-over between contexts: the goal, the
// constraints, decisions/findings so far, and what remains.
type ContextSummary struct {
	Goal        string   `json:"goal"`
	Constraints []string `json:"constraints"`
	Decisions   []string `json:"decisions"`
	Remaining   string   `json:"remaining"`
}

// String renders the summary for embedding into a prompt.
func (s ContextSummary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Goal: %s\n", s.Goal)
	if len(s.Constraints) > 0 {
		fmt.Fprintf(&b, "Constraints:\n- %s\n", strings.Join(s.Constraints, "\n- "))
	}
	if len(s.Decisions) > 0 {
		fmt.Fprintf(&b, "Decisions so far:\n- %s\n", strings.Join(s.Decisions, "\n- "))
	}
	if s.Remaining != "" {
		fmt.Fprintf(&b, "Remaining: %s\n", s.Remaining)
	}
	return b.String()
}

const sysPrompt = `Distill the working state into a compact JSON handoff with exactly these keys:
{"goal": string, "constraints": [string], "decisions": [string], "remaining": string}.
"decisions" are concrete findings/choices already made; "remaining" is what is left.
Respond with ONLY the JSON object.`

// Summarize asks the model to distill goal + working state into a ContextSummary.
// If the model's output is not parseable JSON, it falls back to a minimal summary
// so a handoff never fails outright.
func Summarize(ctx context.Context, m model.Provider, goal, workState string) (ContextSummary, error) {
	user := "Goal:\n" + goal + "\n\nWorking state:\n" + workState
	msgs := []model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: user}}}}
	resp, err := m.Complete(ctx, sysPrompt, msgs, nil, 1024)
	if err != nil {
		return ContextSummary{}, fmt.Errorf("summarize: %w", err)
	}

	text := firstText(resp.Content)
	if cs, ok := parse(text); ok {
		if cs.Goal == "" {
			cs.Goal = goal
		}
		return cs, nil
	}
	// Fallback: never fail a handoff outright.
	return ContextSummary{Goal: goal, Remaining: tail(workState, 500)}, nil
}

func firstText(blocks []model.Block) string {
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			return b.Text
		}
	}
	return ""
}

// parse extracts the first JSON object from s and unmarshals it.
func parse(s string) (ContextSummary, bool) {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return ContextSummary{}, false
	}
	var cs ContextSummary
	if err := json.Unmarshal([]byte(s[start:end+1]), &cs); err != nil {
		return ContextSummary{}, false
	}
	return cs, true
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
