package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/trigger"
)

// pollSignals reads each file as a signal (trimmed contents = goal), hands it to
// the trigger, and removes it so a signal fires exactly once; blanks are skipped.
func TestPollSignals(t *testing.T) {
	dir := t.TempDir()
	log, err := eventlog.Open(filepath.Join(t.TempDir(), "ev.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	var started []string
	trig := &trigger.Trigger{
		Enabled: true,
		Gate:    func(string) bool { return true }, // approve any irreversible signal
		Start:   func(_ context.Context, goal string) error { started = append(started, goal); return nil },
		Log:     log,
	}

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sig-a", "  add a unit test  ") // trimmed goal
	write("empty", "   ")                 // blank → skipped, but still consumed

	pollSignals(context.Background(), trig, dir)

	if len(started) != 1 || started[0] != "add a unit test" {
		t.Fatalf("started = %v, want [\"add a unit test\"]", started)
	}
	for _, n := range []string{"sig-a", "empty"} {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("signal %q must be removed after processing", n)
		}
	}

	// A disabled trigger starts nothing (Handle short-circuits).
	off := &trigger.Trigger{Enabled: false, Start: func(context.Context, string) error {
		t.Error("disabled trigger must not start work")
		return nil
	}, Log: log}
	write("sig-b", "do something")
	pollSignals(context.Background(), off, dir)
}
