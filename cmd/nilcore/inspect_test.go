package main

import (
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/inspect"
)

func TestRenderInspect(t *testing.T) {
	s := inspect.Summary{
		Total:  5,
		ByKind: map[string]int{"task_start": 1, "tool_exec": 3, "verify": 1},
		Tasks:  []string{"t-1", "t-2"},
	}
	out := renderInspect("ev.jsonl", s)
	for _, want := range []string{"ev.jsonl", "5 event(s) across 2 task(s)", "tool_exec", "t-1", "chain: verified"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q in:\n%s", want, out)
		}
	}
}

// End to end: a real hash-chained log replays into a Summary the command renders.
func TestInspectReplaysRealLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	log.Append(eventlog.Event{Task: "t-1", Kind: "task_start"})
	log.Append(eventlog.Event{Task: "t-1", Kind: "tool_exec"})
	log.Append(eventlog.Event{Task: "t-2", Kind: "verify"})
	log.Close()

	sum, err := inspect.Replay(path)
	if err != nil {
		t.Fatalf("replay a valid log: %v", err)
	}
	if sum.Total != 3 || len(sum.Tasks) != 2 || sum.ByKind["tool_exec"] != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if out := renderInspect(path, sum); !strings.Contains(out, "3 event(s)") {
		t.Errorf("render: %s", out)
	}
}
