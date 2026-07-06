package inspect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
)

// buildLog appends the given events to a fresh log file and returns its path.
func buildLog(t *testing.T, events []eventlog.Event) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	for _, e := range events {
		log.Append(e)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	return path
}

func TestReplaySummary(t *testing.T) {
	path := buildLog(t, []eventlog.Event{
		{Task: "P6-T07", Kind: "task_start"},
		{Task: "P6-T07", Kind: "model_call"},
		{Task: "P6-T07", Kind: "tool_exec"},
		{Task: "P6-T08", Kind: "task_start"},
		{Task: "P6-T08", Kind: "model_call"},
	})

	s, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay good log: %v", err)
	}

	if s.Total != 5 {
		t.Errorf("Total = %d, want 5", s.Total)
	}
	wantKind := map[string]int{"task_start": 2, "model_call": 2, "tool_exec": 1}
	for k, want := range wantKind {
		if got := s.ByKind[k]; got != want {
			t.Errorf("ByKind[%q] = %d, want %d", k, got, want)
		}
	}
	if len(s.ByKind) != len(wantKind) {
		t.Errorf("ByKind has %d kinds, want %d: %v", len(s.ByKind), len(wantKind), s.ByKind)
	}

	// Distinct tasks, in first-seen order.
	wantTasks := []string{"P6-T07", "P6-T08"}
	if len(s.Tasks) != len(wantTasks) {
		t.Fatalf("Tasks = %v, want %v", s.Tasks, wantTasks)
	}
	for i, want := range wantTasks {
		if s.Tasks[i] != want {
			t.Errorf("Tasks[%d] = %q, want %q", i, s.Tasks[i], want)
		}
	}
}

func TestHealthOnGoodLog(t *testing.T) {
	path := buildLog(t, []eventlog.Event{
		{Task: "t1", Kind: "verify"},
		{Task: "t1", Kind: "gate"},
	})
	if err := Health(path); err != nil {
		t.Fatalf("Health on a good log: %v", err)
	}
}

func TestReplayDetectsCorruption(t *testing.T) {
	path := buildLog(t, []eventlog.Event{
		{Task: "t1", Kind: "step", Detail: map[string]any{"i": 0}},
		{Task: "t1", Kind: "step", Detail: map[string]any{"i": 1}},
		{Task: "t1", Kind: "step", Detail: map[string]any{"i": 2}},
	})

	// Hand-corrupt the middle event's payload: the hash chain must catch it.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"i":1`, `"i":99`, 1)
	if tampered == string(data) {
		t.Fatal("test setup: nothing replaced")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Replay(path); err == nil {
		t.Error("Replay should error on a corrupted chain")
	}
	if err := Health(path); err == nil {
		t.Error("Health should fail on a corrupted chain")
	}
}

func TestReplayMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	if _, err := Replay(missing); err == nil {
		t.Error("Replay should error when the log file is absent")
	}
	if err := Health(missing); err == nil {
		t.Error("Health should fail when the log file is absent")
	}
}

func TestReplayLongLineExceedingScannerCap(t *testing.T) {
	// A single event whose JSON line exceeds 1MB. eventlog's own reader/Verify use
	// os.ReadFile + strings.Split with no line-length cap, so this line is valid and
	// its hash chain checks out. Replay must accept it too — the old 1MB bufio.Scanner
	// cap would reject a line the chain verifies, a false "unhealthy" report.
	big := strings.Repeat("x", 1500*1024) // ~1.5MB payload > the former 1MB cap
	path := buildLog(t, []eventlog.Event{
		{Task: "t1", Kind: "tool_exec", Detail: map[string]any{"blob": big}},
		{Task: "t1", Kind: "verify"},
	})

	s, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay on a valid >1MB line: %v", err)
	}
	if s.Total != 2 {
		t.Errorf("Total = %d, want 2", s.Total)
	}
	if s.ByKind["tool_exec"] != 1 || s.ByKind["verify"] != 1 {
		t.Errorf("ByKind = %v, want one tool_exec + one verify", s.ByKind)
	}
	if err := Health(path); err != nil {
		t.Errorf("Health on a valid >1MB line: %v", err)
	}
}

func TestReplayEmptyLog(t *testing.T) {
	// An empty log is readable and trivially verifies: zero events, zero tasks.
	path := buildLog(t, nil)
	s, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay empty log: %v", err)
	}
	if s.Total != 0 {
		t.Errorf("Total = %d, want 0", s.Total)
	}
	if len(s.Tasks) != 0 {
		t.Errorf("Tasks = %v, want none", s.Tasks)
	}
	if err := Health(path); err != nil {
		t.Errorf("Health on an empty log: %v", err)
	}
}
