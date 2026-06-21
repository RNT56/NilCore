package browsersession

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"nilcore/internal/browserwire"
	"nilcore/internal/sandbox"
)

// transport is the host↔daemon channel. It is an interface so the Session logic is
// unit-testable against a fake; the production implementation is fileTransport.
type transport interface {
	waitReady(ctx context.Context) error
	send(ctx context.Context, req browserwire.SessionRequest) (browserwire.SessionResponse, error)
	close() error
}

// fileTransport drives the in-sandbox `nilcore-browser --serve` daemon over a
// file-queue on the shared /work mount. The daemon runs in ONE long-lived box.Exec
// (in a goroutine); requests/responses are atomically-written JSON files the other
// side polls for. This works identically under the container bind-mount and the
// namespace shared FS, needs no networking, and is the portable way to keep a
// browser alive across the host's many turns.
type fileTransport struct {
	controlAbs string // host path to the control dir (== /work/.nilcore/browse/<id> in the box)
	seq        int

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	daemonE error // set when the daemon Exec returns (e.g. it exited early)
}

const (
	readyMarker  = "ready"
	reqPrefix    = "req-"
	respPrefix   = "resp-"
	jsonSuffix   = ".json"
	pollInterval = 40 * time.Millisecond
	readyTimeout = 60 * time.Second
	sendTimeout  = 90 * time.Second
)

// newFileTransport creates the control dir and starts the daemon Exec in a
// goroutine. The daemon's cwd in the box is /work, so we pass a control path
// RELATIVE to /work and resolve the same files at box.Workdir()+rel host-side.
func newFileTransport(ctx context.Context, box sandbox.Sandbox, driver, id, initialURL string) (*fileTransport, error) {
	rel := filepath.Join(".nilcore", "browse", id)
	abs := filepath.Join(box.Workdir(), rel)
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("creating control dir: %w", err)
	}

	// Build the daemon command. The control path and URL are harness/task data;
	// single-quote them so nothing can break out of `sh -c` (I4 quoting boundary).
	cmd := fmt.Sprintf("%s --serve --control %s", driver, browserwire.ShellSingleQuote(rel))
	if u := initialURL; u != "" {
		cmd += " --url " + browserwire.ShellSingleQuote(u)
	}

	daemonCtx, cancel := context.WithCancel(context.Background())
	ft := &fileTransport{controlAbs: abs, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(ft.done)
		// box.Exec blocks for the lifetime of the daemon; a non-zero exit or error
		// is recorded so waitReady/send can fail closed instead of hanging.
		res, err := box.Exec(daemonCtx, cmd)
		ft.mu.Lock()
		if err != nil {
			ft.daemonE = err
		} else if res.ExitCode != 0 {
			detail := res.Stderr
			if detail == "" {
				detail = fmt.Sprintf("exit %d", res.ExitCode)
			}
			ft.daemonE = fmt.Errorf("browser daemon exited: %s", detail)
		} else {
			ft.daemonE = fmt.Errorf("browser daemon exited")
		}
		ft.mu.Unlock()
	}()
	return ft, nil
}

func (t *fileTransport) daemonErr() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.daemonE
}

// waitReady polls for the daemon's ready marker, failing fast if the daemon Exec
// returns first (it crashed/exited).
func (t *fileTransport) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(readyTimeout)
	marker := filepath.Join(t.controlAbs, readyMarker)
	for {
		if _, err := os.Stat(marker); err == nil {
			return nil
		}
		if e := t.daemonErr(); e != nil {
			return e
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", readyTimeout)
		}
		if err := t.sleep(ctx); err != nil {
			return err
		}
	}
}

// send writes one request file and polls for its matching response. Strict
// request/response ordering (one in-flight seq) keeps the protocol trivially
// correct; the response file appears atomically (the daemon renames it in).
func (t *fileTransport) send(ctx context.Context, req browserwire.SessionRequest) (browserwire.SessionResponse, error) {
	t.seq++
	seq := t.seq
	req.Seq = seq

	body, err := json.Marshal(req)
	if err != nil {
		return browserwire.SessionResponse{}, fmt.Errorf("marshaling request: %w", err)
	}
	reqPath := filepath.Join(t.controlAbs, fmt.Sprintf("%s%d%s", reqPrefix, seq, jsonSuffix))
	if err := atomicWrite(reqPath, body); err != nil {
		return browserwire.SessionResponse{}, fmt.Errorf("writing request: %w", err)
	}

	respPath := filepath.Join(t.controlAbs, fmt.Sprintf("%s%d%s", respPrefix, seq, jsonSuffix))
	deadline := time.Now().Add(sendTimeout)
	for {
		data, rerr := os.ReadFile(respPath) //nolint:gosec // control path we own
		if rerr == nil {
			var resp browserwire.SessionResponse
			if err := json.Unmarshal(data, &resp); err != nil {
				return browserwire.SessionResponse{}, fmt.Errorf("decoding response: %w", err)
			}
			_ = os.Remove(respPath)
			return resp, nil
		}
		if !os.IsNotExist(rerr) {
			return browserwire.SessionResponse{}, fmt.Errorf("reading response: %w", rerr)
		}
		if e := t.daemonErr(); e != nil {
			return browserwire.SessionResponse{}, e
		}
		if time.Now().After(deadline) {
			return browserwire.SessionResponse{}, fmt.Errorf("browser act timed out after %s", sendTimeout)
		}
		if err := t.sleep(ctx); err != nil {
			return browserwire.SessionResponse{}, err
		}
	}
}

// close best-effort sends a close act (so the daemon shuts Chrome down cleanly),
// cancels the daemon Exec, waits briefly for it, and removes the control dir.
func (t *fileTransport) close() error {
	// Best-effort graceful close with a short bound so a wedged daemon can't block.
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _ = t.send(cctx, browserwire.SessionRequest{Act: browserwire.Act{Op: browserwire.OpClose}})
	cancel()

	t.cancel() // kill the daemon Exec (container teardown) if it is still up
	select {
	case <-t.done:
	case <-time.After(5 * time.Second):
	}
	return os.RemoveAll(t.controlAbs)
}

// sleep waits one poll interval, honoring ctx.
func (t *fileTransport) sleep(ctx context.Context) error {
	tm := time.NewTimer(pollInterval)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tm.C:
		return nil
	}
}

// atomicWrite writes data to a temp file in the same dir and renames it into place.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
