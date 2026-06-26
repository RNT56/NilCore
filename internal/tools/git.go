package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
)

// GitTool runs a constrained set of git operations in the worktree. Only the
// listed subcommands are allowed (no push/merge/reset) so the operation surface
// stays inspectable and reversible; the integration gate handles irreversible
// git actions at the orchestrator level. The read-only history ops (blame, show)
// add NO mutation surface — they let the agent read WHY/WHEN a line exists
// ("understand before you change") without granting any write power.
type GitTool struct{}

func (GitTool) Name() string { return "git" }
func (GitTool) Description() string {
	return "Run a git operation in the working directory: status | diff | add | commit | log | blame | show."
}
func (GitTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"op":{"type":"string","enum":["status","diff","add","commit","log","blame","show"]},"paths":{"type":"array","items":{"type":"string"}},"message":{"type":"string"},"path":{"type":"string"},"line_range":{"type":"string"},"rev":{"type":"string"}},"required":["op"]}`)
}

// blameRange validates a `git blame -L` range: "N" or "N,M" (digits only). Anything
// else is rejected so a model-supplied range can never smuggle a flag or path.
var blameRange = regexp.MustCompile(`^\d+(,\d+)?$`)

// safeRev permits only the characters a revision/ref needs (alnum plus ./_-^~), and
// never a leading dash — so `rev` can never be read as a flag.
var safeRev = regexp.MustCompile(`^[A-Za-z0-9_./^~-]+$`)

func (GitTool) Run(ctx context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Op        string   `json:"op"`
		Paths     []string `json:"paths"`
		Message   string   `json:"message"`
		Path      string   `json:"path"`
		LineRange string   `json:"line_range"`
		Rev       string   `json:"rev"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}

	// HardenArgs prefixes every invocation with `-c` flags that neutralize
	// repo-controlled code-execution vectors. A model can write into the worktree
	// (incl. .git/hooks and .git/config), so committing must not let an attacker-
	// authored hook or fsmonitor binary run on the host. (See HardenedEnv for the
	// matching environment clamp; both live in githard.go and are shared with
	// host-side worktree/integration git.)
	hardenArgs := HardenArgs()

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
	case "blame":
		// Read-only: who/what last touched each line of a file (optionally a range).
		if in.Path == "" {
			return "", fmt.Errorf("blame requires a path")
		}
		args = []string{"blame"}
		if in.LineRange != "" {
			if !blameRange.MatchString(in.LineRange) {
				return "", fmt.Errorf("blame line_range must be N or N,M")
			}
			args = append(args, "-L", in.LineRange)
		}
		// `--` ends option parsing so the path can never be read as a flag.
		args = append(args, "--", in.Path)
	case "show":
		// Read-only: a commit/object (default HEAD), summarized with --stat to bound
		// output. rev is strictly validated so it can never be read as a flag.
		rev := in.Rev
		if rev == "" {
			rev = "HEAD"
		}
		if !safeRev.MatchString(rev) {
			return "", fmt.Errorf("show rev contains disallowed characters")
		}
		args = []string{"show", "--stat", rev}
	default:
		return "", fmt.Errorf("unsupported git op %q", in.Op)
	}

	cmd := exec.CommandContext(ctx, "git", append(hardenArgs, args...)...)
	cmd.Dir = workdir
	cmd.Env = HardenedEnv()
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
