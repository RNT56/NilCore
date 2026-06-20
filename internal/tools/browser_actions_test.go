package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nilcore/internal/sandbox"
)

// capBox records the command it was asked to run so the test can assert how the
// flow is shelled out.
type capBox struct {
	result sandbox.Result
	cmd    string
}

func (b *capBox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	b.cmd = cmd
	return b.result, nil
}
func (b *capBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return b.Exec(ctx, cmd)
}
func (b *capBox) Workdir() string { return "" }

func TestHasActions(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{``, false},
		{`null`, false},
		{`[]`, false},
		{`{"action":"click"}`, false}, // object, not an array
		{`[{"action":"click","selector":"#a"}]`, true},
	}
	for _, c := range cases {
		if got := hasActions(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("hasActions(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestFlowModeBuildsActionsCommand(t *testing.T) {
	box := &capBox{result: sandbox.Result{Stdout: `{"title":"Dashboard","text":"ok","screenshot_b64":"UE5H"}`, ExitCode: 0}}
	in := `{"url":"http://localhost:3000/","actions":[{"action":"click","selector":"#login"},{"action":"type","selector":"#u","text":"alice"}]}`
	out, img, err := BrowserViewTool{Box: box}.RunWithImage(context.Background(), ".", json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(box.cmd, "--actions ") {
		t.Fatalf("flow mode must shell --actions; cmd = %q", box.cmd)
	}
	if !strings.Contains(box.cmd, "--url 'http://localhost:3000/'") {
		t.Errorf("a provided url should seed the flow; cmd = %q", box.cmd)
	}
	if img == nil || img.Base64 != "UE5H" {
		t.Errorf("flow mode should still deliver the screenshot image, got %+v", img)
	}
	if !strings.Contains(out, "Dashboard") {
		t.Errorf("observation missing; out = %q", out)
	}
}

func TestFlowModeShellQuotesActions(t *testing.T) {
	// A selector containing a single quote must be escaped so it cannot break out of
	// the `--actions '...'` quoting (I7: actions are data, never shell).
	box := &capBox{result: sandbox.Result{Stdout: `{"title":"t"}`, ExitCode: 0}}
	in := `{"actions":[{"action":"click","selector":"a'); rm -rf / #"}]}`
	if _, _, err := (BrowserViewTool{Box: box}).RunWithImage(context.Background(), ".", json.RawMessage(in)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(box.cmd, `'\''`) == false {
		t.Errorf("embedded single-quote must be escaped as '\\'' in the command; cmd = %q", box.cmd)
	}
	// The command must remain a single safely-quoted --actions argument: every
	// single-quote is part of the escaping, so the shell sees one argument.
	if strings.Count(box.cmd, "rm -rf") != 1 || !strings.Contains(box.cmd, "--actions '") {
		t.Errorf("actions payload not safely contained; cmd = %q", box.cmd)
	}
}

func TestNoActionsIsByteIdenticalViewCommand(t *testing.T) {
	box := &capBox{result: sandbox.Result{Stdout: `{"title":"t"}`, ExitCode: 0}}
	if _, _, err := (BrowserViewTool{Box: box}).RunWithImage(context.Background(), ".", json.RawMessage(`{"url":"http://x.test/"}`)); err != nil {
		t.Fatal(err)
	}
	if box.cmd != "nilcore-browser --url 'http://x.test/' --format json" {
		t.Errorf("view-mode command changed: %q", box.cmd)
	}
}
