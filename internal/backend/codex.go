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
type Codex struct {
	Box sandbox.Sandbox
	Key string // CODEX_API_KEY, injected per run; never logged
	Log *eventlog.Log
}

func (c *Codex) Name() string { return "codex" }

func (c *Codex) Run(ctx context.Context, t Task) (Result, error) {
	if err := ensureCLI(ctx, c.Box, "codex"); err != nil {
		return Result{Backend: c.Name()}, err
	}
	cmd := "codex exec --json --full-auto " + shellQuote(t.Goal)
	out, err := c.Box.ExecWithEnv(ctx, cmd, map[string]string{"CODEX_API_KEY": c.Key})
	if err != nil {
		return Result{Backend: c.Name()}, fmt.Errorf("codex exec: %w", err)
	}
	// Log the run WITHOUT the key (only the exit code and that codex ran).
	c.Log.Append(eventlog.Event{Task: t.ID, Backend: c.Name(), Kind: "tool_exec",
		Detail: map[string]any{"cli": "codex", "exit": out.ExitCode}})
	if out.ExitCode != 0 {
		return Result{Backend: c.Name(), Summary: tailStr(out.Stderr, 500)}, nil
	}
	return Result{Backend: c.Name(), Summary: lastEventText(out.Stdout), SelfClaimed: true}, nil
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

// digText walks a few common shapes (text/message/delta, and nested
// params/item objects) to find a text payload.
func digText(m map[string]any) string {
	for _, k := range []string{"text", "message", "delta"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	for _, k := range []string{"params", "item", "msg"} {
		if sub, ok := m[k].(map[string]any); ok {
			if s := digText(sub); s != "" {
				return s
			}
		}
	}
	return ""
}

func tailStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
