package browsersession

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilcore/internal/browserwire"
	"nilcore/internal/sandbox"
)

// fakeDaemonBox is a sandbox.Sandbox whose Exec plays the role of the in-sandbox
// nilcore-browser --serve daemon: it parses --control, writes the ready marker,
// and runs the file-queue request/response loop in-process — exercising the REAL
// fileTransport (newFileTransport/waitReady/send/close) end-to-end with no Chrome
// and no container. Workdir() returns the shared dir (the host's view of /work).
type fakeDaemonBox struct{ dir string }

func (f *fakeDaemonBox) Workdir() string { return f.dir }
func (f *fakeDaemonBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return f.Exec(ctx, cmd)
}
func (f *fakeDaemonBox) Exec(ctx context.Context, cmd string) (sandbox.Result, error) {
	control := f.controlFromCmd(cmd)
	if control == "" {
		return sandbox.Result{ExitCode: 2, Stderr: "no --control"}, nil
	}
	runFakeDaemon(ctx, control)
	return sandbox.Result{ExitCode: 0}, nil
}

// controlFromCmd extracts the (single-quoted, /work-relative) control path and
// resolves it against the shared dir, mirroring how the real daemon resolves it
// against its cwd (/work == Workdir()).
func (f *fakeDaemonBox) controlFromCmd(cmd string) string {
	toks := strings.Fields(cmd)
	for i, t := range toks {
		if t == "--control" && i+1 < len(toks) {
			rel := strings.Trim(toks[i+1], "'")
			return filepath.Join(f.dir, rel)
		}
	}
	return ""
}

// runFakeDaemon is the in-process stand-in for serve.go's serveLoop: ready marker,
// then one scripted Observation per request until a close act or ctx cancellation.
func runFakeDaemon(ctx context.Context, control string) {
	_ = os.MkdirAll(control, 0o700)
	_ = atomicWrite(filepath.Join(control, readyMarker), []byte("1"))
	seq := 1
	for {
		if ctx.Err() != nil {
			return
		}
		reqPath := filepath.Join(control, reqPrefix+itoa(seq)+jsonSuffix)
		data, err := os.ReadFile(reqPath)
		if err != nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		var req browserwire.SessionRequest
		_ = json.Unmarshal(data, &req)
		if req.Act.Op == browserwire.OpClose {
			_ = atomicWrite(filepath.Join(control, respPrefix+itoa(seq)+jsonSuffix),
				mustJSON(browserwire.SessionResponse{Seq: seq}))
			_ = os.Remove(reqPath)
			return
		}
		// Echo the op into the URL and always offer one ref, so ref-based acts validate.
		obs := browserwire.Observation{
			Version: uint64(seq),
			URL:     "http://x.test/" + req.Act.Op,
			Refs:    []browserwire.Ref{{ID: 1, Role: "button", Name: "Go"}},
		}
		_ = atomicWrite(filepath.Join(control, respPrefix+itoa(seq)+jsonSuffix),
			mustJSON(browserwire.SessionResponse{Seq: seq, Observation: obs}))
		_ = os.Remove(reqPath)
		seq++
	}
}

func itoa(n int) string     { return string(rune('0' + n)) } // single-digit seqs suffice for these tests
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestFileTransportRoundTrip(t *testing.T) {
	box := &fakeDaemonBox{dir: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, first, err := Launch(ctx, box, Options{InitialURL: "http://x.test/"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer sess.Close()

	// The first observation came from the Observe inside Launch (the daemon's seq 1).
	if first.URL == "" || len(first.Refs) == 0 {
		t.Fatalf("first observation empty: %+v", first)
	}

	// A navigate act round-trips through the real file-queue.
	obs, err := sess.Act(ctx, browserwire.Act{Op: browserwire.OpNavigate, URL: "http://y.test/"})
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if !strings.Contains(obs.URL, "navigate") {
		t.Fatalf("navigate observation = %q, want it to reflect the op", obs.URL)
	}

	// A ref-based click validates against the latest snapshot (which carries ref 1).
	if _, err := sess.Act(ctx, browserwire.Act{Op: browserwire.OpClick, Ref: 1}); err != nil {
		t.Fatalf("click ref 1: %v", err)
	}

	// A ref absent from the latest snapshot fails closed BEFORE hitting the queue.
	if _, err := sess.Act(ctx, browserwire.Act{Op: browserwire.OpClick, Ref: 7}); err == nil {
		t.Fatal("expected a stale-ref guard error for ref 7")
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestLaunchNilBoxRefuses(t *testing.T) {
	if _, _, err := Launch(context.Background(), nil, Options{}); err == nil {
		t.Fatal("Launch with a nil sandbox must refuse (no host-side browser)")
	}
}
