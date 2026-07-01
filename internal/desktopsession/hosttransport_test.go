package desktopsession

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nilcore/internal/desktopwire"
)

// TestMain lets this test binary impersonate the host desktop-driver subprocess: when
// LaunchHost execs us with `--serve --control <dir>` we play a scripted file-queue
// daemon and exit; otherwise we run the tests normally. This is the standard os/exec
// subprocess-test pattern, and it validates the REAL hostTransport (subprocess launch
// + file-queue) without the live macOS driver.
func TestMain(m *testing.M) {
	if len(os.Args) >= 2 && os.Args[1] == "--serve" {
		runTestDaemon(os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

func runTestDaemon(args []string) {
	var control string
	for i, a := range args {
		if a == "--control" && i+1 < len(args) {
			control = args[i+1]
		}
	}
	if control == "" {
		os.Exit(2)
	}
	_ = os.MkdirAll(control, 0o700)
	_ = atomicWrite(filepath.Join(control, readyMarker), []byte("1"))
	seq := 1
	for {
		reqPath := filepath.Join(control, reqPrefix+itoaT(seq)+jsonSuffix)
		data, err := os.ReadFile(reqPath)
		if err != nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		var req desktopwire.SessionRequest
		_ = json.Unmarshal(data, &req)
		if req.Act.Op == desktopwire.OpClose {
			b, _ := json.Marshal(desktopwire.SessionResponse{Seq: seq})
			_ = atomicWrite(filepath.Join(control, respPrefix+itoaT(seq)+jsonSuffix), b)
			_ = os.Remove(reqPath)
			os.Exit(0)
		}
		obs := desktopwire.Observation{Version: uint64(seq), Rung: desktopwire.RungSoM,
			Refs: []desktopwire.Ref{{ID: 1, Role: "element", Version: uint64(seq)}}}
		b, _ := json.Marshal(desktopwire.SessionResponse{Seq: seq, Observation: obs})
		_ = atomicWrite(filepath.Join(control, respPrefix+itoaT(seq)+jsonSuffix), b)
		_ = os.Remove(reqPath)
		seq++
	}
}

func itoaT(n int) string { return string(rune('0' + n)) } // single-digit seqs suffice

func TestLaunchHostRoundTrip(t *testing.T) {
	// Driver = this test binary; TestMain plays the daemon.
	s, first, err := LaunchHost(context.Background(), HostOptions{Driver: os.Args[0]})
	if err != nil {
		t.Fatalf("LaunchHost: %v", err)
	}
	defer s.Close()
	if first.Rung != desktopwire.RungSoM || len(first.Refs) != 1 {
		t.Fatalf("first observation wrong: %+v", first)
	}
	// A ref-based act validates against the snapshot and round-trips over the real
	// host-subprocess file-queue.
	if _, err := s.Act(context.Background(), desktopwire.Act{Op: desktopwire.OpClick, Ref: 1}); err != nil {
		t.Fatalf("click ref 1: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestLaunchHostBogusDriverFailsClosed(t *testing.T) {
	if _, _, err := LaunchHost(context.Background(), HostOptions{Driver: "/nonexistent/nilcore-desktop-xyz"}); err == nil {
		t.Fatal("a missing driver binary must fail closed")
	}
}
