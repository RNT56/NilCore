// Package advisor is the strong-model tier the cheap executor consults on demand
// (Anthropic's Advisor Strategy, P3-T08): when the executor is stuck, above its
// skill, or needs a plan, it calls the `ask_advisor` tool; the harness seeds the
// advisor with a ContextSummary, returns its guidance, and the executor resumes.
// The advisor advises only — it never executes. A per-task call ceiling bounds
// cost, and a harness fallback escalates after K consecutive verifier failures.
// The advisor tier doubles as the planner (P3-T01) and the cross-model reviewer.
package advisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"nilcore/internal/model"
	"nilcore/internal/summarize"
)

// ErrCeiling is returned when the per-task advisor-call ceiling is reached.
var ErrCeiling = errors.New("advisor call ceiling reached")

// Advisor wraps the strong advisor model with a call ceiling.
type Advisor struct {
	Model    model.Provider
	MaxCalls int // per-task ceiling (0 = unlimited)
	calls    int
}

// New returns an advisor over m with the given per-task call ceiling.
func New(m model.Provider, maxCalls int) *Advisor { return &Advisor{Model: m, MaxCalls: maxCalls} }

// Calls reports how many advisor calls have been made this task.
func (a *Advisor) Calls() int { return a.calls }

const sysPrompt = `You are a senior engineering advisor. The executor is stuck, above its skill, or needs a plan.
Given the context summary and the question, give concise, actionable guidance — a plan, a correction, or a clear "stop and ask the human".
You advise only; you do not execute.`

// Consult escalates to the advisor with a context summary and a focused question.
// It enforces the call ceiling.
func (a *Advisor) Consult(ctx context.Context, summary summarize.ContextSummary, question string) (string, error) {
	if a.MaxCalls > 0 && a.calls >= a.MaxCalls {
		return "", ErrCeiling
	}
	a.calls++
	user := "Context:\n" + summary.String() + "\nQuestion:\n" + question
	resp, err := a.Model.Complete(ctx, sysPrompt,
		[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: user}}}}, nil, 1024)
	if err != nil {
		return "", fmt.Errorf("advisor consult: %w", err)
	}
	return firstText(resp.Content), nil
}

// Tool is the `ask_advisor` tool the executor registers in its loop. Calling it
// escalates to the advisor tier (the self-built path; Anthropic's native Advisor
// Tool is a config toggle that uses the same Messages client).
func Tool() model.Tool {
	return model.Tool{
		Name:        "ask_advisor",
		Description: "Escalate to a strong advisor model when stuck, above your skill, or needing a plan. Provide a focused question.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"}},"required":["question"]}`),
	}
}

// ShouldEscalate reports whether the harness should auto-consult the advisor after
// consecutiveFailures verifier failures — the fallback path that escalates even
// when the executor does not ask.
func ShouldEscalate(consecutiveFailures, k int) bool {
	return k > 0 && consecutiveFailures >= k
}

func firstText(blocks []model.Block) string {
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			return b.Text
		}
	}
	return ""
}
