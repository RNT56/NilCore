package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nilcore/internal/eventlog"
	"nilcore/internal/guard"
	"nilcore/internal/model"
	"nilcore/internal/sandbox"
	"nilcore/internal/tools"
	"nilcore/internal/verify"
)

// Native is nilcore's own coding loop: the model proposes a shell action, the
// sandbox runs it, the verifier judges, and the loop repeats until the checks
// pass or the step budget is exhausted. This is the frozen core contract —
// capability grows around it, not inside it.
type Native struct {
	Model    model.Provider
	Box      sandbox.Sandbox
	Verifier verify.Verifier
	Log      *eventlog.Log
	Tools    *tools.Registry // optional structured tools; nil = shell-only

	// CommandGuard vets each `run` shell command before it executes (P2-T04).
	// nil allows everything; a denied call returns a structured error to the
	// model and is never run. Wired to policy.CommandPolicy.Check.
	CommandGuard func(cmd string) (allowed bool, reason string)

	MaxSteps int // tool-call ceiling (budget). Generous by default.
}

func (n *Native) Name() string { return "native" }

const systemPrompt = `You are nilcore's native coding worker. You operate inside a sandboxed
working directory via the "run" tool, which executes shell commands and returns
stdout, stderr, and the exit code. Make the smallest change that satisfies the
goal. Inspect files before editing them. When you believe the goal is met and
the project's checks should pass, call the "finish" tool with a short summary.
Do not ask the user questions; act.`

func (n *Native) Run(ctx context.Context, t Task) (Result, error) {
	steps := n.MaxSteps
	if steps <= 0 {
		steps = 60 // generous: optimize for finishing
	}

	toolDefs := []model.Tool{
		{
			Name:        "run",
			Description: "Run a shell command in the working directory. Returns stdout, stderr, exit code.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}`),
		},
		{
			Name:        "finish",
			Description: "Declare the goal complete. Provide a one-paragraph summary of what changed.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
		},
	}
	// Structured tools (read/write/edit/search/git) load from the registry, so
	// adding a tool never edits this loop; shell ("run") stays the fallback.
	if n.Tools != nil {
		toolDefs = append(n.Tools.Defs(), toolDefs...)
	}

	user := "Goal:\n" + t.Goal
	if len(t.Constraints) > 0 {
		user += "\n\nConstraints:\n- " + strings.Join(t.Constraints, "\n- ")
	}
	msgs := []model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: user}}}}

	for i := 0; i < steps; i++ {
		resp, err := n.Model.Complete(ctx, systemPrompt, msgs, toolDefs, 4096)
		if err != nil {
			return Result{Backend: n.Name()}, fmt.Errorf("model step %d: %w", i, err)
		}
		n.Log.Append(eventlog.Event{Task: t.ID, Backend: n.Name(), Kind: "model_call",
			Detail: map[string]any{"step": i, "stop": resp.StopReason, "out_tokens": resp.Usage.OutputTokens}})

		// Record the assistant turn verbatim so the conversation stays coherent.
		msgs = append(msgs, model.Message{Role: "assistant", Content: resp.Content})

		// The API requires a tool_result for every tool_use block, so we build
		// one per block — including "finish" — before deciding what to do next.
		var results []model.Block
		finished := false
		var summary string

		for _, b := range resp.Content {
			if b.Type != "tool_use" {
				continue
			}
			switch b.Name {
			case "finish":
				var in struct {
					Summary string `json:"summary"`
				}
				_ = json.Unmarshal(b.Input, &in)
				summary, finished = in.Summary, true
				results = append(results, model.Block{Type: "tool_result", ToolUseID: b.ID, Content: "noted"})

			case "run":
				var in struct {
					Cmd string `json:"cmd"`
				}
				if err := json.Unmarshal(b.Input, &in); err != nil {
					results = append(results, errorResult(b.ID, "bad input: "+err.Error()))
					continue
				}
				if n.CommandGuard != nil {
					if ok, reason := n.CommandGuard(in.Cmd); !ok {
						n.Log.Append(eventlog.Event{Task: t.ID, Backend: n.Name(), Kind: "tool_denied",
							Detail: map[string]any{"cmd": in.Cmd, "reason": reason}})
						results = append(results, errorResult(b.ID, reason))
						continue
					}
				}
				out, err := n.Box.Exec(ctx, in.Cmd)
				if err != nil {
					results = append(results, errorResult(b.ID, "sandbox error: "+err.Error()))
					continue
				}
				n.Log.Append(eventlog.Event{Task: t.ID, Backend: n.Name(), Kind: "tool_exec",
					Detail: map[string]any{"cmd": in.Cmd, "exit": out.ExitCode}})
				rendered := render(out)
				if guard.Suspicious(rendered) {
					n.Log.Append(eventlog.Event{Task: t.ID, Backend: n.Name(), Kind: "injection_flagged",
						Detail: map[string]any{"source": "shell output"}})
				}
				// Untrusted boundary (I7): fence tool output as data, never instructions.
				results = append(results, model.Block{Type: "tool_result", ToolUseID: b.ID, Content: guard.Wrap("shell output", rendered)})

			default:
				if n.Tools != nil && n.Tools.Has(b.Name) {
					out, err := n.Tools.Dispatch(ctx, b.Name, t.Dir, b.Input)
					if err != nil {
						results = append(results, errorResult(b.ID, b.Name+": "+err.Error()))
						continue
					}
					n.Log.Append(eventlog.Event{Task: t.ID, Backend: n.Name(), Kind: "tool_exec",
						Detail: map[string]any{"tool": b.Name}})
					results = append(results, model.Block{Type: "tool_result", ToolUseID: b.ID, Content: guard.Wrap(b.Name+" output", out)})
					continue
				}
				results = append(results, errorResult(b.ID, "unknown tool: "+b.Name))
			}
		}

		if finished {
			// Source of truth: the model's claim does not decide completion.
			rep, err := n.Verifier.Check(ctx)
			if err != nil {
				return Result{Backend: n.Name(), Summary: summary}, fmt.Errorf("verify: %w", err)
			}
			n.Log.Append(eventlog.Event{Task: t.ID, Backend: n.Name(), Kind: "verify",
				Detail: map[string]any{"passed": rep.Passed}})
			if rep.Passed {
				return Result{Backend: n.Name(), Summary: summary, SelfClaimed: true}, nil
			}
			// Not actually done: return the tool_results for this turn plus the
			// failure, in one user message, and keep going.
			fail := append(results, model.Block{
				Type: "text",
				Text: "The checks did not pass. Fix the issues and call finish again.\n\n" + guard.Wrap("verifier output", rep.Output),
			})
			msgs = append(msgs, model.Message{Role: "user", Content: fail})
			continue
		}

		if len(results) == 0 {
			// The model talked without acting; nudge it once.
			msgs = append(msgs, model.Message{Role: "user", Content: []model.Block{{
				Type: "text",
				Text: "No tool call detected. Use run to act, or finish when done.",
			}}})
			continue
		}
		msgs = append(msgs, model.Message{Role: "user", Content: results})
	}

	return Result{Backend: n.Name(), Summary: "budget exhausted before completion"}, nil
}

func errorResult(id, msg string) model.Block {
	return model.Block{Type: "tool_result", ToolUseID: id, Content: msg, IsError: true}
}

func render(out sandbox.Result) string {
	return fmt.Sprintf("exit=%d\n--- stdout ---\n%s\n--- stderr ---\n%s", out.ExitCode, out.Stdout, out.Stderr)
}
