package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Codex delegates the task to OpenAI's Codex CLI in non-interactive mode:
//
//	codex exec --json --full-auto "<goal>"
//
// It streams JSONL events on stdout; we keep the last human-readable message as
// the summary. In production this command runs *inside* the sandbox container
// (Codex sandboxes itself too — defense in depth) with CODEX_API_KEY injected
// for the single run. Phase 0 ships the exec + parse seam; richer event
// handling (per-event file-change tracking) is a Phase 1 detail.
type Codex struct {
	Bin string // defaults to "codex"
}

func (c *Codex) Name() string { return "codex" }

func (c *Codex) Run(ctx context.Context, t Task) (Result, error) {
	bin := c.Bin
	if bin == "" {
		bin = "codex"
	}
	cmd := exec.CommandContext(ctx, bin, "exec", "--json", "--full-auto", t.Goal)
	cmd.Dir = t.Dir // codex exec requires a git repo; the worktree is one

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Result{Backend: c.Name()}, fmt.Errorf("codex exec: %w (%s)", err, tailStr(stderr.String(), 500))
	}
	return Result{Backend: c.Name(), Summary: lastEventText(stdout.String()), SelfClaimed: true}, nil
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
