package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"nilcore/internal/desktopwire"
)

// TestRunServeRoundTrip exercises the FULL driver serve loop end-to-end over the
// real file-queue, with every live X11 seam faked (no Xvfb/scrot/xdotool/AT-SPI).
// It proves the daemon: writes ready, observes via the ladder, applies an act, and
// shuts down on close — the same path the host-side desktopsession drives.
func TestRunServeRoundTrip(t *testing.T) {
	restore := withSeams(t, `[{"role":"push button","name":"OK","box":{"x":5,"y":5,"w":20,"h":10}}]`, nil, "win", nil)
	defer restore()
	od := ensureDisplay
	ensureDisplay = func(context.Context) error { return nil } // skip Xvfb startup
	defer func() { ensureDisplay = od }()

	control := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, control, false) }()

	// The daemon writes a ready marker once it is serving.
	waitFor(t, filepath.Join(control, readyMarker))

	// seq 1: observe → Rung 1 (the faked a11y has one ref).
	writeReq(t, control, 1, desktopwire.Act{Op: desktopwire.OpObserve})
	r1 := readResp(t, control, 1)
	if r1.Error != "" {
		t.Fatalf("observe error: %s", r1.Error)
	}
	if r1.Observation.Rung != desktopwire.RungATSPI || len(r1.Observation.Refs) != 1 || r1.Observation.Refs[0].Name != "OK" {
		t.Fatalf("observe obs wrong: %+v", r1.Observation)
	}

	// seq 2: click the ref → still succeeds, re-observes.
	writeReq(t, control, 2, desktopwire.Act{Op: desktopwire.OpClick, Ref: 1})
	r2 := readResp(t, control, 2)
	if r2.Error != "" {
		t.Fatalf("click error: %s", r2.Error)
	}

	// seq 3: close → the daemon shuts down.
	writeReq(t, control, 3, desktopwire.Act{Op: desktopwire.OpClose})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServe returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServe did not exit on close")
	}
}

func writeReq(t *testing.T, control string, seq int, a desktopwire.Act) {
	t.Helper()
	b, _ := json.Marshal(desktopwire.SessionRequest{Seq: seq, Act: a})
	if err := atomicWrite(filepath.Join(control, reqPrefix+itoa(seq)+jsonSuffix), b); err != nil {
		t.Fatal(err)
	}
}

func readResp(t *testing.T, control string, seq int) desktopwire.SessionResponse {
	t.Helper()
	p := filepath.Join(control, respPrefix+itoa(seq)+jsonSuffix)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(p); err == nil {
			var r desktopwire.SessionResponse
			if json.Unmarshal(data, &r) == nil {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("response %d never appeared", seq)
	return desktopwire.SessionResponse{}
}

func waitFor(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

func itoa(n int) string { return strconv.Itoa(n) }
