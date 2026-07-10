package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nilcore/internal/eventlog"
	"nilcore/internal/sandbox"
)

// Codex delegates the task to OpenAI's Codex CLI in non-interactive mode:
//
//	codex exec --json --full-auto "<goal>"
//
// It runs *inside* the sandbox container (defense in depth — Codex sandboxes
// itself too) with CODEX_API_KEY injected per run (P2-T03): the key reaches the
// container only for this invocation and is never written to disk, logged, or put
// in a prompt (invariant I3). It streams JSONL on stdout; the last human-readable
// message becomes the summary.
//
// The Model/Effort/ExtraArgs/Env fields are additive operator knobs (R1): with
// all four zero the emitted command and env are byte-identical to the historical
// default, so existing behavior is unchanged. Every interpolated value flows
// through shellQuote, so a model id or extra arg containing a space, quote, or
// semicolon stays a single argument and can never break out of `sh -c`.
type Codex struct {
	Box sandbox.Sandbox
	Key string // CODEX_API_KEY, injected per run; never logged
	Log *eventlog.Log

	Model     string            // delegated model id; "" = the CLI's default
	Effort    string            // reasoning effort; "" = default
	ExtraArgs []string          // raw operator-supplied extra CLI tokens, appended verbatim
	Env       map[string]string // extra per-run env merged with the API key; values never logged (I3)
}

func (c *Codex) Name() string { return "codex" }

func (c *Codex) Run(ctx context.Context, t Task) (Result, error) {
	if err := ensureCLI(ctx, c.Box, "codex"); err != nil {
		return Result{Backend: c.Name()}, err
	}
	cmd := codexArgs(t.Goal, c.Model, c.Effort, c.ExtraArgs)
	// The injected CODEX_API_KEY wins (merged last) so an operator's Env can never
	// shadow the per-run key.
	env := mergeEnv(c.Env, "CODEX_API_KEY", c.Key)
	out, err := c.Box.ExecWithEnv(ctx, cmd, env)
	if err != nil {
		return Result{Backend: c.Name()}, fmt.Errorf("codex exec: %w", err)
	}
	// Log the run WITHOUT the key, model, effort, or env (only the exit code and
	// that codex ran) — invariant I3.
	c.Log.Append(eventlog.Event{Task: t.ID, Backend: c.Name(), Kind: "tool_exec",
		Detail: map[string]any{"cli": "codex", "exit": out.ExitCode}})
	if out.ExitCode != 0 {
		return Result{Backend: c.Name(), Summary: tailStr(out.Stderr, 500)}, nil
	}
	return Result{Backend: c.Name(), Summary: lastEventText(out.Stdout), SelfClaimed: true}, nil
}

// codexArgs builds the full `sh -c` command for the Codex CLI. It is a pure
// helper (no sandbox, no env) so command construction is unit-testable in
// isolation. With model/effort/extra all zero it returns exactly
// `codex exec --json --full-auto '<goal>'` — byte-identical to the original.
// Every operator-supplied value is shellQuote'd, so it stays one shell argument.
func codexArgs(goal, model, effort string, extra []string) string {
	cmd := "codex exec --json --full-auto"
	if model != "" {
		cmd += " --model " + shellQuote(model)
	}
	if effort != "" {
		cmd += " -c model_reasoning_effort=" + shellQuote(effort)
	}
	for _, a := range extra {
		cmd += " " + shellQuote(a)
	}
	return cmd + " " + shellQuote(goal)
}

// mergeEnv returns a fresh map of base plus the (key, val) pair, with the pair
// applied LAST so the per-run secret always wins over any operator override. A
// nil base yields a one-entry map; base is never mutated.
func mergeEnv(base map[string]string, key, val string) map[string]string {
	merged := make(map[string]string, len(base)+1)
	for k, v := range base {
		merged[k] = v
	}
	merged[key] = val
	return merged
}

// shellQuote single-quotes s for safe use in `sh -c`.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ensureCLI fails fast if the delegated CLI is not present in the sandbox image.
// The delegated backends run the CLI *inside* the container, so this is the check
// that matters — a clear, actionable error beats a cryptic "command not found"
// surfacing as a failed task.
func ensureCLI(ctx context.Context, box sandbox.Sandbox, cli string) error {
	out, err := box.Exec(ctx, "command -v "+cli)
	if err != nil {
		return fmt.Errorf("checking for the %s CLI in the sandbox: %w", cli, err)
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("the %q CLI is not installed in the sandbox image; add it to the image or use -backend native", cli)
	}
	return nil
}

// lastEventText extracts the final human-readable text from a JSONL event
// stream, falling back to the tail of raw output if the shape is unfamiliar.
// Shared by the Codex and Claude Code adapters.
func lastEventText(jsonl string) string {
	var last string
	for _, line := range strings.Split(strings.TrimSpace(jsonl), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if s := digText(ev); s != "" {
			last = s
		}
	}
	if last == "" {
		return tailStr(jsonl, 800)
	}
	return last
}

// digText walks the shapes the delegated CLIs actually emit to find the human-readable
// text payload:
//
//   - Codex: an item.completed event carries item.text (a string); some events carry a
//     top-level text/delta string. The item/msg/params descent handles the nesting.
//   - Claude Code stream-json: the FINAL answer rides a result event under the
//     top-level "result" STRING key ({"type":"result","subtype":"success","result":...}),
//     and an assistant event carries message.content as an ARRAY of
//     {type:"text",text:...} blocks (message is an OBJECT, not a string). Earlier this
//     handled neither, so every real claude-code run's Summary degraded to a raw-JSONL
//     tail. digText now reads the result string, descends the message object, and
//     extracts text from a content[] array.
func digText(m map[string]any) string {
	// Direct string payloads: Codex text/delta; Claude Code's result-event "result".
	// "message" stays here too for the rare event that carries it as a plain string.
	for _, k := range []string{"text", "result", "message", "delta"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	// A content[] array of blocks (Claude Code message.content): join its text blocks.
	if s := textFromContent(m["content"]); s != "" {
		return s
	}
	// Nested objects: Claude Code's message object, Codex's item/msg/params.
	for _, k := range []string{"message", "params", "item", "msg"} {
		if sub, ok := m[k].(map[string]any); ok {
			if s := digText(sub); s != "" {
				return s
			}
		}
	}
	return ""
}

// textFromContent extracts and concatenates the text of a content array as emitted by
// Claude Code (message.content = [{type:"text",text:"..."}, ...]). Non-text blocks
// (tool_use / tool_result) are skipped — their bodies are not the readable summary. A
// non-array, or an array with no text blocks, yields "".
func textFromContent(v any) string {
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, el := range arr {
		blk, ok := el.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := blk["type"].(string); typ != "" && typ != "text" {
			continue
		}
		if s, ok := blk["text"].(string); ok && s != "" {
			b.WriteString(s)
		}
	}
	return b.String()
}

func tailStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
