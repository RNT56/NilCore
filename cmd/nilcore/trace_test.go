package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/trace"
)

// seedTraceLog writes a hash-chained event log describing two tasks: t-1 runs a
// model turn + a tool exec and verifies green; t-2 verifies red. trace.Build/BuildAll
// reconstruct the causal tree from these, and the chain verifies (real writer).
func seedTraceLog(t *testing.T) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log.Append(eventlog.Event{Task: "t-1", Kind: "task_start", Detail: map[string]any{"goal": "fix the failing test"}})
	log.Append(eventlog.Event{Task: "t-1", Backend: "native", Kind: "model_call", Detail: map[string]any{"step": 1}})
	log.Append(eventlog.Event{Task: "t-1", Backend: "native", Kind: "tool_exec", Detail: map[string]any{"tool": "edit"}})
	log.Append(eventlog.Event{Task: "t-1", Backend: "native", Kind: "final_verify", Detail: map[string]any{"passed": true}})

	log.Append(eventlog.Event{Task: "t-2", Kind: "task_start", Detail: map[string]any{"goal": "add a feature"}})
	log.Append(eventlog.Event{Task: "t-2", Backend: "native", Kind: "model_call", Detail: map[string]any{"step": 1}})
	log.Append(eventlog.Event{Task: "t-2", Backend: "native", Kind: "final_verify", Detail: map[string]any{"passed": false}})
	log.Close()
	return logPath
}

// breakTraceChain forges a line so eventlog.Verify fails: the trace LEAF still
// renders structurally but marks the trace untrusted (ChainVerified == false).
func breakTraceChain(t *testing.T, logPath string) {
	t.Helper()
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":99,"task":"t-1","kind":"final_verify","detail":{"passed":true},"hash":"deadbeef"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestTraceSingleTaskText(t *testing.T) {
	logPath := seedTraceLog(t)
	out, exit, err := runTrace(logPath, "t-1", "text", plainStyle(t))
	if err != nil {
		t.Fatalf("runTrace: %v", err)
	}
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 (clean chain)", exit)
	}
	for _, want := range []string{"why:", "t-1", "fix the failing test", "chain verified"} {
		if !strings.Contains(out, want) {
			t.Errorf("trace missing %q:\n%s", want, out)
		}
	}
	// A single-task trace must not bleed in the other task's goal.
	if strings.Contains(out, "add a feature") {
		t.Errorf("single-task trace leaked another task's goal:\n%s", out)
	}
}

func TestTraceAllTasks(t *testing.T) {
	logPath := seedTraceLog(t)
	// No task arg ⇒ BuildAll: every task is rendered.
	out, exit, err := runTrace(logPath, "", "text", plainStyle(t))
	if err != nil {
		t.Fatalf("runTrace all: %v", err)
	}
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	for _, want := range []string{"t-1", "t-2", "fix the failing test", "add a feature"} {
		if !strings.Contains(out, want) {
			t.Errorf("all-tasks trace missing %q:\n%s", want, out)
		}
	}
}

func TestTraceStarMeansAll(t *testing.T) {
	logPath := seedTraceLog(t)
	star, _, err := runTrace(logPath, "*", "text", plainStyle(t))
	if err != nil {
		t.Fatalf("runTrace *: %v", err)
	}
	empty, _, err := runTrace(logPath, "", "text", plainStyle(t))
	if err != nil {
		t.Fatalf("runTrace empty: %v", err)
	}
	if star != empty {
		t.Errorf(`"*" and "" should both mean all-tasks and render identically`)
	}
}

func TestTraceJSON(t *testing.T) {
	logPath := seedTraceLog(t)
	// A specific task ⇒ a single Trace object.
	out, _, err := runTrace(logPath, "t-1", "json", plainStyle(t))
	if err != nil {
		t.Fatalf("runTrace json: %v", err)
	}
	var tr trace.Trace
	if err := json.Unmarshal([]byte(out), &tr); err != nil {
		t.Fatalf("single-task json should be a Trace object: %v\n%s", err, out)
	}
	if tr.Task != "t-1" {
		t.Errorf("want task t-1, got %q", tr.Task)
	}
	if !tr.ChainVerified {
		t.Errorf("clean chain should be ChainVerified in JSON: %+v", tr)
	}

	// No task ⇒ an array of Traces.
	outAll, _, err := runTrace(logPath, "", "json", plainStyle(t))
	if err != nil {
		t.Fatalf("runTrace json all: %v", err)
	}
	var traces []trace.Trace
	if err := json.Unmarshal([]byte(outAll), &traces); err != nil {
		t.Fatalf("all-tasks json should be a Trace array: %v\n%s", err, outAll)
	}
	if len(traces) != 2 {
		t.Errorf("want 2 traces, got %d", len(traces))
	}
}

func TestTraceBrokenChainRendersButExitsNonZero(t *testing.T) {
	logPath := seedTraceLog(t)
	breakTraceChain(t, logPath)
	out, exit, err := runTrace(logPath, "t-1", "text", plainStyle(t))
	// I5: a broken chain STILL renders the structure (no error), but is flagged
	// untrusted and exits non-zero so a script detects the tampering.
	if err != nil {
		t.Fatalf("a broken chain should still render (no fatal error): %v", err)
	}
	if exit == 0 {
		t.Fatal("a broken chain must exit non-zero (untrusted)")
	}
	if !strings.Contains(out, "CHAIN NOT VERIFIED") && !strings.Contains(out, "CHAIN BROKEN") {
		t.Errorf("broken-chain trace must surface the untrusted banner:\n%s", out)
	}
}

func TestTraceBrokenChainJSONExitsNonZero(t *testing.T) {
	logPath := seedTraceLog(t)
	breakTraceChain(t, logPath)
	out, exit, err := runTrace(logPath, "", "json", plainStyle(t))
	if err != nil {
		t.Fatalf("broken chain json should still render: %v", err)
	}
	if exit == 0 {
		t.Fatal("broken-chain json must exit non-zero")
	}
	var traces []trace.Trace
	if err := json.Unmarshal([]byte(out), &traces); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	for _, tr := range traces {
		if tr.ChainVerified {
			t.Errorf("every trace over a broken chain must be ChainVerified=false: %+v", tr)
		}
	}
}

func TestTraceMissingLogErrors(t *testing.T) {
	// Unlike trust (missing ⇒ empty), the trace builder treats an absent log as
	// unreadable — there is no run to explain — so it is a fatal error.
	if _, _, err := runTrace(filepath.Join(t.TempDir(), "absent.jsonl"), "t-1", "text", plainStyle(t)); err == nil {
		t.Fatal("a missing log must error for trace")
	}
}

func TestTraceUnknownFormatErrors(t *testing.T) {
	if _, _, err := runTrace(seedTraceLog(t), "t-1", "xml", plainStyle(t)); err == nil {
		t.Fatal("unknown -format must error")
	}
}

func TestSplitLeadingTask(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantTask string
		wantRest []string
	}{
		{"task then flags", []string{"t-1", "--log", "x"}, "t-1", []string{"--log", "x"}},
		{"flags only", []string{"--log", "x"}, "", []string{"--log", "x"}},
		{"task only", []string{"t-1"}, "t-1", []string{}},
		{"star task", []string{"*", "--format", "json"}, "*", []string{"--format", "json"}},
		{"empty", nil, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task, rest := splitLeadingTask(c.args)
			if task != c.wantTask {
				t.Errorf("task = %q, want %q", task, c.wantTask)
			}
			if strings.Join(rest, " ") != strings.Join(c.wantRest, " ") {
				t.Errorf("rest = %v, want %v", rest, c.wantRest)
			}
		})
	}
}

func TestWhyIsTraceAlias(t *testing.T) {
	// `why` dispatches to the SAME traceMain; runTrace is the shared core, so a `why`
	// invocation produces byte-identical output to `trace` for the same args. We assert
	// the core is shared by rendering once (the alias is wired in main.go's switch).
	logPath := seedTraceLog(t)
	out, _, err := runTrace(logPath, "t-1", "text", plainStyle(t))
	if err != nil {
		t.Fatalf("runTrace: %v", err)
	}
	if !strings.Contains(out, "why:") {
		t.Errorf("the why/trace core must render the why header:\n%s", out)
	}
}
