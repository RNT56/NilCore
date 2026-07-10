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

	box := &fakeBox{stdout: `{"type":"result","subtype":"success","result":"changed files"}`}
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

	// REAL Claude Code stream-json shapes: an assistant event carries message.content
	// as an ARRAY of {type:"text",text:...} blocks; the final result event carries the
	// answer under the top-level "result" STRING key. (The prior fixtures used invented
	// shapes — message.text / a result event with a "text" key — that the CLI never
	// emits, so the parser only worked by accident.)
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"abc","tools":["Edit"]}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"working on it"}]}}`,
		`{"type":"result","subtype":"success","result":"all done"}`,
	}, "\n")
	box := &fakeBox{stdout: stream}
	cc := &ClaudeCode{Box: box, Key: "k", Log: log}
	res, err := cc.Run(context.Background(), Task{ID: "t3", Goal: "do it"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "all done" {
		t.Errorf("summary = %q, want the result event's answer past the init framing", res.Summary)
	}
	if !strings.Contains(box.gotCmd, "--verbose") {
		t.Errorf("emitted command lacks --verbose: %q", box.gotCmd)
	}
}

// TestLastEventTextRealCLIShapes covers FIX #3: lastEventText/digText must handle the
// REAL stream-json shapes the delegated CLIs emit — Claude Code's result event (answer
// under the top-level "result" key) and assistant events (message.content[] array of
// text blocks) — while keeping Codex's item.completed → item.text path intact. Before
// this, a real claude-code run's Summary degraded to a raw-JSONL tail.
func TestLastEventTextRealCLIShapes(t *testing.T) {
	t.Run("claude-code result + assistant content array", func(t *testing.T) {
		stream := strings.Join([]string{
			`{"type":"system","subtype":"init","session_id":"s"}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":"editing files"}]}}`,
			`{"type":"result","subtype":"success","result":"done: 2 files changed"}`,
		}, "\n")
		if got := lastEventText(stream); got != "done: 2 files changed" {
			t.Errorf("lastEventText = %q, want the result-event answer", got)
		}
	})

	t.Run("claude-code assistant content array alone", func(t *testing.T) {
		// No result event: the last assistant message.content[] text is the summary.
		stream := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello world"},{"type":"tool_use","id":"x","name":"Edit","input":{}}]}}`
		if got := lastEventText(stream); got != "hello world" {
			t.Errorf("lastEventText = %q, want the text block (tool_use skipped)", got)
		}
	})

	t.Run("codex item.completed path preserved", func(t *testing.T) {
		stream := strings.Join([]string{
			`{"type":"item.started","item":{"type":"reasoning"}}`,
			`{"type":"item.completed","item":{"type":"agent_message","text":"codex summary"}}`,
		}, "\n")
		if got := lastEventText(stream); got != "codex summary" {
			t.Errorf("lastEventText = %q, want Codex's item.text", got)
		}
	})

	t.Run("unfamiliar shape falls back to tail", func(t *testing.T) {
		// No recognizable text payload ⇒ the raw tail, not the empty string.
		stream := `{"type":"mystery","blob":{"nested":123}}`
		if got := lastEventText(stream); got != stream {
			t.Errorf("lastEventText = %q, want the raw tail fallback", got)
		}
	})
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
