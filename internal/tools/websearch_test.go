package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nilcore/internal/sandbox"
)

// envBox records both the command and the per-run env so the web_search tests can
// assert the anti-leak property: the API key reaches the box ONLY via env, never in
// the command string the loop would log.
type envBox struct {
	cmd    string
	env    map[string]string
	result sandbox.Result
	err    error
}

func (b *envBox) Exec(ctx context.Context, cmd string) (sandbox.Result, error) {
	return b.ExecWithEnv(ctx, cmd, nil)
}
func (b *envBox) ExecWithEnv(_ context.Context, cmd string, env map[string]string) (sandbox.Result, error) {
	b.cmd, b.env = cmd, env
	return b.result, b.err
}
func (b *envBox) Workdir() string { return "/work" }

// The headline I3 test: the literal API key must NOT appear in the command string
// (which the loop logs), only in the per-run env (which the sandbox never logs).
func TestWebSearchDoesNotLeakKeyIntoCommand(t *testing.T) {
	const key = "brave-secret-key-123"
	box := &envBox{result: sandbox.Result{Stdout: `{"web":{"results":[]}}`, ExitCode: 0}}
	tool := WebSearchTool{Box: box, APIKey: key}

	out, err := tool.Run(context.Background(), "", json.RawMessage(`{"query":"golang context window"}`))
	if err != nil {
		t.Fatalf("web_search: %v", err)
	}
	if strings.Contains(box.cmd, key) {
		t.Fatalf("API KEY LEAKED into the command string (the loop logs this): %q", box.cmd)
	}
	if box.env["NILCORE_SEARCH_KEY"] != key {
		t.Errorf("key not passed via per-run env: %v", box.env)
	}
	if !strings.Contains(box.cmd, "$NILCORE_SEARCH_KEY") {
		t.Errorf("command should reference the key via $env, got %q", box.cmd)
	}
	// Ran a curl against the Brave host with the escaped query.
	if !strings.Contains(box.cmd, searchHost) || !strings.Contains(box.cmd, "golang+context+window") {
		t.Errorf("search did not curl the API host with the escaped query: %q", box.cmd)
	}
	// Results are fenced as untrusted data (I7).
	if !strings.Contains(out, "UNTRUSTED DATA") {
		t.Errorf("results not guard.Wrap-fenced:\n%s", out)
	}
}

func TestWebSearchRefusesWithoutBoxOrKey(t *testing.T) {
	if _, err := (WebSearchTool{APIKey: "k"}).Run(context.Background(), "", json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Error("web_search with nil box must refuse (no host fallback)")
	}
	box := &envBox{result: sandbox.Result{ExitCode: 0}}
	if _, err := (WebSearchTool{Box: box}).Run(context.Background(), "", json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Error("web_search with no key must refuse")
	}
}

func TestWebSearchRejectsControlCharsAndEmptyQuery(t *testing.T) {
	box := &envBox{result: sandbox.Result{ExitCode: 0}}
	tool := WebSearchTool{Box: box, APIKey: "k"}
	if _, err := tool.Run(context.Background(), "", json.RawMessage(`{"query":"  "}`)); err == nil {
		t.Error("empty query must be rejected")
	}
	if _, err := tool.Run(context.Background(), "", json.RawMessage(`{"query":"a\nb"}`)); err == nil {
		t.Error("query with a newline must be rejected")
	}
}

// A non-zero curl exit (egress-blocked host, auth failure) is surfaced as fenced
// data, not a Go error — the model reacts without being instructed by the remote.
func TestWebSearchFencesErrorBody(t *testing.T) {
	box := &envBox{result: sandbox.Result{Stderr: "curl: (22) 403", ExitCode: 22}}
	out, err := (WebSearchTool{Box: box, APIKey: "k"}).Run(context.Background(), "", json.RawMessage(`{"query":"x"}`))
	if err != nil {
		t.Fatalf("a non-zero exit should be a fenced result, not an error: %v", err)
	}
	if !strings.Contains(out, "UNTRUSTED DATA") {
		t.Errorf("error body not fenced:\n%s", out)
	}
}
