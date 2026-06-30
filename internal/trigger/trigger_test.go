package trigger

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
)

func TestReversibleSelfStarts(t *testing.T) {
	var startedGoal string
	tr := &Trigger{
		Enabled: true,
		Gate:    func(string) bool { return false }, // would deny, but reversible shouldn't ask
		Start:   func(_ context.Context, goal string) error { startedGoal = goal; return nil },
	}
	started, err := tr.Handle(context.Background(), Signal{Source: "ci", Goal: "fix the failing test in math_test.go"})
	if err != nil || !started {
		t.Fatalf("reversible signal should self-start: %v %v", started, err)
	}
	if startedGoal == "" {
		t.Error("Start was not called")
	}
}

func TestIrreversibleGated(t *testing.T) {
	denied := &Trigger{
		Enabled: true,
		Gate:    func(string) bool { return false },
		Start:   func(context.Context, string) error { t := false; _ = t; return nil },
	}
	started, _ := denied.Handle(context.Background(), Signal{Source: "ci", Goal: "git push origin main"})
	if started {
		t.Error("irreversible work must be gated; a denied gate must not start it")
	}

	var ran bool
	approved := &Trigger{
		Enabled: true,
		Gate:    func(string) bool { return true },
		Start:   func(context.Context, string) error { ran = true; return nil },
	}
	started, _ = approved.Handle(context.Background(), Signal{Goal: "deploy to staging"})
	if !started || !ran {
		t.Error("an approved gate should let irreversible work start")
	}
}

func TestDisabledDoesNothing(t *testing.T) {
	tr := &Trigger{Enabled: false, Start: func(context.Context, string) error { panic("must not start") }}
	if started, _ := tr.Handle(context.Background(), Signal{Goal: "anything"}); started {
		t.Error("disabled trigger must not start work")
	}
}

// TestNilStartDoesNotClaimStarted proves that when no runnable Start is wired,
// Handle reports started=false (nothing ran) AND emits no trigger_start event,
// so neither the return value nor the audit trail claims work that never began.
func TestNilStartDoesNotClaimStarted(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}

	tr := &Trigger{
		Enabled: true,
		Gate:    func(string) bool { return true }, // gate would pass; Start is nil
		Start:   nil,
		Log:     log,
	}
	// A reversible goal (no gate involved) with a nil Start must not claim a start.
	started, err := tr.Handle(context.Background(), Signal{Source: "ci", Goal: "fix the failing test"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if started {
		t.Error("a nil Start must report started=false; nothing ran")
	}

	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	if got := countEvents(t, logPath, "trigger_start"); got != 0 {
		t.Errorf("nil Start must emit no trigger_start event, got %d", got)
	}
}

// countEvents counts append-only events of the given Kind in a JSONL event log.
func countEvents(t *testing.T, path, kind string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log for read: %v", err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"kind":"`+kind+`"`) {
			n++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan log: %v", err)
	}
	return n
}
