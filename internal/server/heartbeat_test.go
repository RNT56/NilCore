package server

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nilcore/internal/eventlog"
	"nilcore/internal/session"
)

// blockingDriver parks a drive until released (or ctx ends), so a Session can be held
// in the Working phase while liveCounts is observed.
type blockingDriver struct{ release chan struct{} }

func (d blockingDriver) Drive(ctx context.Context, _ session.DriveInput) (session.DriveResult, error) {
	select {
	case <-d.release:
	case <-ctx.Done():
	}
	return session.DriveResult{}, nil
}

// The heartbeat emits a serve_heartbeat pulse at its cadence carrying live counts
// (one idle + one working thread ⇒ threads=2, working=1), and exits cleanly on ctx
// cancellation (hbWG.Wait returns — no leak).
func TestHeartbeatEmitsLivenessPulse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// The parked drive gets its OWN context, so cancelling the heartbeat (cancel) does
	// NOT release the drive: the blockingDriver selects on ctx.Done(), so sharing one ctx
	// let `cancel()` unblock BOTH the heartbeat and the drive — and a final heartbeat beat
	// racing ctx.Done() vs a ready tick could then observe the drive already back to Idle
	// (working=0), a flake under CI load. With a separate driveCtx the drive stays parked
	// (on rel) across every beat, so the in-flight count is stable.
	driveCtx, driveCancel := context.WithCancel(context.Background())
	defer driveCancel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}

	srv := &Server{Log: log, threads: map[string]*thread{}}

	// One idle thread.
	idle := session.New("idle", "u", "", nil)
	srv.threads["idle"] = &thread{sess: idle}

	// One working thread: route to a driver that blocks, then wait until it reports Working.
	rel := make(chan struct{})
	work := session.New("work", "u", "", nil)
	work.Router = stubRouter{}
	work.Drivers = session.Drivers{Native: blockingDriver{release: rel}}
	srv.threads["work"] = &thread{sess: work}
	if err := work.Turn(driveCtx, "go"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for work.PhaseNow() == session.Idle && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if work.PhaseNow() == session.Idle {
		t.Fatal("working thread never left Idle")
	}

	// Run the heartbeat fast; let it tick a few times, then cancel and join.
	srv.hbWG.Add(1)
	go srv.runHeartbeat(ctx, 10*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	cancel()
	srv.hbWG.Wait() // returns ⇒ the goroutine exited on ctx (no leak)

	close(rel)  // release the parked drive
	work.Wait() // join it so nothing writes after we read the log
	log.Close()

	// Read back: at least one serve_heartbeat must have observed threads=2 + working=1
	// (the in-flight drive) with an uptime field. We assert on SOME beat rather than the
	// LAST one: the drive is parked across every beat (its ctx outlives the heartbeat), so
	// every beat sees working=1 — but asserting "some beat" stays robust to any transient
	// phase read at the exact tick boundary.
	beats := readHeartbeats(t, logPath)
	if len(beats) == 0 {
		t.Fatal("no serve_heartbeat events emitted")
	}
	var saw bool
	for _, b := range beats {
		if _, ok := b["uptime_seconds"]; !ok {
			t.Error("heartbeat missing uptime_seconds")
		}
		if b["threads"] == float64(2) && b["working"] == float64(1) {
			saw = true
		}
	}
	if !saw {
		t.Errorf("no heartbeat observed threads=2 + working=1 (the in-flight drive); beats=%v", beats)
	}
}

// liveCounts on an all-idle server reports working=0.
func TestLiveCountsAllIdle(t *testing.T) {
	srv := &Server{threads: map[string]*thread{
		"a": {sess: session.New("a", "u", "", nil)},
		"b": {sess: session.New("b", "u", "", nil)},
	}}
	threads, working := srv.liveCounts()
	if threads != 2 || working != 0 {
		t.Errorf("liveCounts = (%d,%d), want (2,0)", threads, working)
	}
}

func readHeartbeats(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log for read: %v", err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e struct {
			Kind   string         `json:"kind"`
			Detail map[string]any `json:"detail"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if e.Kind == "serve_heartbeat" {
			out = append(out, e.Detail)
		}
	}
	return out
}
