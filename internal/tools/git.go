package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
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

	// hardenArgs prefixes every invocation with `-c` flags that neutralize
	// repo-controlled code-execution vectors. A model can write into the worktree
	// (incl. .git/hooks and .git/config), so committing must not let an attacker-
	// authored hook or fsmonitor binary run on the host. (See hardenedGitEnv for
	// the matching environment clamp.)
	hardenArgs := []string{
		"-c", "core.hooksPath=/dev/null", // disable all repo hooks (pre-commit, etc.)
		"-c", "core.fsmonitor=", // disable any fsmonitor hook binary
	}

	var args []string
	switch in.Op {
	case "status":
		args = []string{"status", "--short"}
	case "diff":
		// `--` ends option parsing: model-supplied paths can never be read as
		// flags (e.g. `--output=/tmp/x` would otherwise exfiltrate the diff).
		args = append([]string{"diff", "--"}, in.Paths...)
	case "add":
		if len(in.Paths) == 0 {
			args = []string{"add", "-A"}
		} else {
			args = append([]string{"add", "--"}, in.Paths...)
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

	cmd := exec.CommandContext(ctx, "git", append(hardenArgs, args...)...)
	cmd.Dir = workdir
	cmd.Env = hardenedGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w", in.Op, err)
	}
	if len(out) == 0 {
		return "(no output)", nil
	}
	return string(out), nil
}

// hardenedGitEnv returns the process environment with every external-config and
// credential-prompt vector stripped, then pinned to inert values: git ignores
// /etc/gitconfig and ~/.gitconfig (which could carry [core] hooksPath, aliases,
// or credential helpers) and never blocks on an interactive prompt. Combined with
// the per-invocation `-c` flags, a repo a model can write to cannot make git run
// arbitrary code on the host.
func hardenedGitEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src)+4)
	for _, e := range src {
		switch {
		case strings.HasPrefix(e, "GIT_CONFIG"),
			strings.HasPrefix(e, "GIT_TERMINAL_PROMPT="),
			strings.HasPrefix(e, "GIT_ALLOW_PROTOCOL="),
			strings.HasPrefix(e, "GIT_PROXY_COMMAND="),
			strings.HasPrefix(e, "GIT_EXTERNAL_DIFF="):
			continue // drop anything that could re-introduce external behavior
		}
		out = append(out, e)
	}
	return append(out,
		"GIT_CONFIG_NOSYSTEM=1",       // ignore /etc/gitconfig
		"GIT_CONFIG_GLOBAL=/dev/null", // ignore ~/.gitconfig
		"GIT_TERMINAL_PROMPT=0",       // never prompt for credentials
	)
}

// Default returns the standard structured tool set the native loop registers.
func Default() *Registry {
	return NewRegistry(ReadTool{}, WriteTool{}, EditTool{}, SearchTool{}, GitTool{})
}
