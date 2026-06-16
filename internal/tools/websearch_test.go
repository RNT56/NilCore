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

// The headline I3 test (brave backend): the literal API key must NOT appear in the
// command string (which the loop logs), only in the per-run env.
func TestWebSearchBraveDoesNotLeakKey(t *testing.T) {
	const key = "brave-secret-key-123"
	box := &envBox{result: sandbox.Result{Stdout: `{"web":{"results":[]}}`, ExitCode: 0}}
	tool := WebSearchTool{Box: box, Backend: SearchBrave, APIKey: key}

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
	if !strings.Contains(box.cmd, SearchHostFor(SearchBrave)) || !strings.Contains(box.cmd, "golang+context+window") {
		t.Errorf("search did not curl the Brave host with the escaped query: %q", box.cmd)
	}
	if !strings.Contains(out, "UNTRUSTED DATA") {
		t.Errorf("results not guard.Wrap-fenced:\n%s", out)
	}
}

// The keyless DDG backend: NO key, NO env, hits the DuckDuckGo Lite host, fences
// the HTML body. This is the no-signup default.
func TestWebSearchDDGKeyless(t *testing.T) {
	box := &envBox{result: sandbox.Result{Stdout: "<html>results</html>", ExitCode: 0}}
	tool := WebSearchTool{Box: box, Backend: SearchDDG} // no APIKey

	out, err := tool.Run(context.Background(), "", json.RawMessage(`{"query":"go testing"}`))
	if err != nil {
		t.Fatalf("ddg web_search: %v", err)
	}
	if len(box.env) != 0 {
		t.Errorf("keyless backend must pass no env, got %v", box.env)
	}
	if !strings.Contains(box.cmd, SearchHostFor(SearchDDG)) || !strings.Contains(box.cmd, "go+testing") {
		t.Errorf("ddg did not curl the lite host with the escaped query: %q", box.cmd)
	}
	if strings.Contains(box.cmd, "Subscription-Token") {
		t.Errorf("keyless backend must not send an auth header: %q", box.cmd)
	}
	if !strings.Contains(out, "UNTRUSTED DATA") {
		t.Errorf("ddg results not fenced:\n%s", out)
	}
}

func TestWebSearchRefusesMisconfigured(t *testing.T) {
	box := &envBox{result: sandbox.Result{ExitCode: 0}}
	// nil box
	if _, err := (WebSearchTool{Backend: SearchDDG}).Run(context.Background(), "", json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Error("nil box must refuse (no host fallback)")
	}
	// brave with no key
	if _, err := (WebSearchTool{Box: box, Backend: SearchBrave}).Run(context.Background(), "", json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Error("brave with no key must refuse")
	}
	// off / unresolved
	if _, err := (WebSearchTool{Box: box, Backend: SearchOff}).Run(context.Background(), "", json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Error("SearchOff must refuse")
	}
	if _, err := (WebSearchTool{Box: box, Backend: SearchAuto}).Run(context.Background(), "", json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Error("unresolved SearchAuto must refuse")
	}
}

func TestWebSearchRejectsBadQuery(t *testing.T) {
	box := &envBox{result: sandbox.Result{ExitCode: 0}}
	tool := WebSearchTool{Box: box, Backend: SearchDDG}
	if _, err := tool.Run(context.Background(), "", json.RawMessage(`{"query":"  "}`)); err == nil {
		t.Error("empty query must be rejected")
	}
	if _, err := tool.Run(context.Background(), "", json.RawMessage(`{"query":"a\nb"}`)); err == nil {
		t.Error("query with a newline must be rejected")
	}
}

func TestWebSearchFencesErrorBody(t *testing.T) {
	box := &envBox{result: sandbox.Result{Stderr: "curl: (22) 403", ExitCode: 22}}
	out, err := (WebSearchTool{Box: box, Backend: SearchDDG}).Run(context.Background(), "", json.RawMessage(`{"query":"x"}`))
	if err != nil {
		t.Fatalf("a non-zero exit should be a fenced result, not an error: %v", err)
	}
	if !strings.Contains(out, "UNTRUSTED DATA") {
		t.Errorf("error body not fenced:\n%s", out)
	}
}

func TestSearchHostFor(t *testing.T) {
	if SearchHostFor(SearchBrave) != "api.search.brave.com" {
		t.Error("brave host")
	}
	if SearchHostFor(SearchDDG) != "lite.duckduckgo.com" {
		t.Error("ddg host")
	}
	if SearchHostFor(SearchOff) != "" || SearchHostFor(SearchAuto) != "" {
		t.Error("off/auto have no fixed host")
	}
}
