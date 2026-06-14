package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

// GitTool runs a constrained set of git operations in the worktree. Only the
// listed subcommands are allowed (no push/merge/reset) so the operation surface
// stays inspectable and reversible; the integration gate handles irreversible
// git actions at the orchestrator level.
type GitTool struct{}

func (GitTool) Name() string { return "git" }
func (GitTool) Description() string {
	return "Run a git operation in the working directory: status | diff | add | commit | log."
}
func (GitTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"op":{"type":"string","enum":["status","diff","add","commit","log"]},"paths":{"type":"array","items":{"type":"string"}},"message":{"type":"string"}},"required":["op"]}`)
}

func (GitTool) Run(ctx context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Op      string   `json:"op"`
		Paths   []string `json:"paths"`
		Message string   `json:"message"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}

	var args []string
	switch in.Op {
	case "status":
		args = []string{"status", "--short"}
	case "diff":
		args = append([]string{"diff"}, in.Paths...)
	case "add":
		if len(in.Paths) == 0 {
			args = []string{"add", "-A"}
		} else {
			args = append([]string{"add"}, in.Paths...)
		}
	case "commit":
		if in.Message == "" {
			return "", fmt.Errorf("commit requires a message")
		}
		args = []string{"-c", "user.email=agent@nilcore.local", "-c", "user.name=nilcore", "commit", "-m", in.Message}
	case "log":
		args = []string{"log", "--oneline", "-n", "20"}
	default:
		return "", fmt.Errorf("unsupported git op %q", in.Op)
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workdir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w", in.Op, err)
	}
	if len(out) == 0 {
		return "(no output)", nil
	}
	return string(out), nil
}

// Default returns the standard structured tool set the native loop registers.
func Default() *Registry {
	return NewRegistry(ReadTool{}, WriteTool{}, EditTool{}, SearchTool{}, GitTool{})
}
