package backend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/sandbox"
)

type fakeBox struct {
	gotCmd string
	gotEnv map[string]string
	stdout string
	exit   int
}

func (f *fakeBox) Exec(ctx context.Context, cmd string) (sandbox.Result, error) {
	return f.ExecWithEnv(ctx, cmd, nil)
}
func (f *fakeBox) ExecWithEnv(_ context.Context, cmd string, env map[string]string) (sandbox.Result, error) {
	f.gotCmd, f.gotEnv = cmd, env
	return sandbox.Result{Stdout: f.stdout, ExitCode: f.exit}, nil
}
func (f *fakeBox) Workdir() string { return "/work" }

func TestCodexInjectsKeyPerRunAndNeverLogsIt(t *testing.T) {
	const secret = "sk-codex-secret-xyz"
	logPath := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	box := &fakeBox{stdout: `{"type":"message","text":"done"}`}
	cx := &Codex{Box: box, Key: secret, Log: log}
	res, err := cx.Run(context.Background(), Task{ID: "t1", Goal: "fix the bug"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if box.gotEnv["CODEX_API_KEY"] != secret {
		t.Errorf("key not injected per run: %v", box.gotEnv)
	}
	if !strings.Contains(box.gotCmd, "codex exec") || !strings.Contains(box.gotCmd, "fix the bug") {
		t.Errorf("unexpected command: %q", box.gotCmd)
	}
	if res.Summary != "done" {
		t.Errorf("summary = %q", res.Summary)
	}

	// The key must never appear in the event log.
	b, _ := os.ReadFile(logPath)
	if strings.Contains(string(b), secret) {
		t.Fatal("secret leaked into the event log")
	}
}

func TestClaudeCodeInjectsKeyPerRunAndNeverLogsIt(t *testing.T) {
	const secret = "sk-ant-secret-xyz"
	logPath := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	box := &fakeBox{stdout: `{"type":"text","text":"changed files"}`}
	cc := &ClaudeCode{Box: box, Key: secret, Log: log}
	if _, err := cc.Run(context.Background(), Task{ID: "t2", Goal: "do it"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if box.gotEnv["ANTHROPIC_API_KEY"] != secret {
		t.Errorf("key not injected per run: %v", box.gotEnv)
	}
	b, _ := os.ReadFile(logPath)
	if strings.Contains(string(b), secret) {
		t.Fatal("secret leaked into the event log")
	}
}

func TestClaudeArgsIncludesVerboseWithStreamJSON(t *testing.T) {
	// Current Claude Code CLIs reject --output-format stream-json under -p unless
	// --verbose is also passed, so the emitted command MUST carry --verbose.
	cmd := claudeArgs("fix the bug", "", nil)
	for _, want := range []string{"--output-format stream-json", "--verbose", "claude -p"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("claudeArgs missing %q: %q", want, cmd)
		}
	}
}

func TestClaudeCodeToleratesVerboseInitFraming(t *testing.T) {
	// With --verbose the stream carries init/system framing lines that have no text
	// payload; the parser must skip them and still surface the last real message.
	logPath := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"abc","tools":["Edit"]}`,
		`{"type":"assistant","message":{"text":"working on it"}}`,
		`{"type":"result","subtype":"success","text":"all done"}`,
	}, "\n")
	box := &fakeBox{stdout: stream}
	cc := &ClaudeCode{Box: box, Key: "k", Log: log}
	res, err := cc.Run(context.Background(), Task{ID: "t3", Goal: "do it"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "all done" {
		t.Errorf("summary = %q, want the last text payload past the init framing", res.Summary)
	}
	if !strings.Contains(box.gotCmd, "--verbose") {
		t.Errorf("emitted command lacks --verbose: %q", box.gotCmd)
	}
}

func TestDelegatedFailsFastWhenCLIMissing(t *testing.T) {
	// fakeBox returns a non-zero exit for everything, so the `command -v` pre-flight
	// reports the CLI is absent and the backend fails fast before running the task.
	cx := &Codex{Box: &fakeBox{exit: 1}, Key: "k"}
	if _, err := cx.Run(context.Background(), Task{ID: "t", Goal: "x"}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("codex: want a clear missing-CLI error, got %v", err)
	}
	cc := &ClaudeCode{Box: &fakeBox{exit: 1}, Key: "k"}
	if _, err := cc.Run(context.Background(), Task{ID: "t", Goal: "x"}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("claude-code: want a clear missing-CLI error, got %v", err)
	}
}
