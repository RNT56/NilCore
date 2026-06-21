package desktopsession

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"nilcore/internal/desktopwire"
	"nilcore/internal/sandbox"
)

// transport is the host↔daemon channel — an interface so the Session logic is
// unit-testable against a fake. The production implementation is fileTransport,
// the desktop sibling of browsersession's file-queue transport.
type transport interface {
	waitReady(ctx context.Context) error
	send(ctx context.Context, req desktopwire.SessionRequest) (desktopwire.SessionResponse, error)
	close() error
}

// fileTransport drives the in-sandbox `nilcore-desktop --serve` daemon over a
// file-queue on the shared /work mount — portable across container + namespace
// backends, no networking, the same pattern as the browser session.
type fileTransport struct {
	controlAbs string
	seq        int

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	daemonE error
}

const (
	readyMarker  = "ready"
	reqPrefix    = "req-"
	respPrefix   = "resp-"
	jsonSuffix   = ".json"
	pollInterval = 40 * time.Millisecond
	readyTimeout = 90 * time.Second // a desktop (Xvfb + WM + apps) is slower to come up than a browser
	sendTimeout  = 120 * time.Second
)

func newFileTransport(ctx context.Context, box sandbox.Sandbox, driver, id string, native bool) (*fileTransport, error) {
	rel := filepath.Join(".nilcore", "desktop", id)
	abs := filepath.Join(box.Workdir(), rel)
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("creating control dir: %w", err)
	}

	cmd := fmt.Sprintf("%s --serve --control %s", driver, desktopwire.ShellSingleQuote(rel))
	if native {
		cmd += " --native"
	}

	daemonCtx, cancel := context.WithCancel(context.Background())
	ft := &fileTransport{controlAbs: abs, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(ft.done)
		res, err := box.Exec(daemonCtx, cmd)
		ft.mu.Lock()
		if err != nil {
			ft.daemonE = err
		} else if res.ExitCode != 0 {
			detail := res.Stderr
			if detail == "" {
				detail = fmt.Sprintf("exit %d", res.ExitCode)
			}
			ft.daemonE = fmt.Errorf("desktop daemon exited: %s", detail)
		} else {
			ft.daemonE = fmt.Errorf("desktop daemon exited")
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

func (t *fileTransport) send(ctx context.Context, req desktopwire.SessionRequest) (desktopwire.SessionResponse, error) {
	t.seq++
	seq := t.seq
	req.Seq = seq

	body, err := json.Marshal(req)
	if err != nil {
		return desktopwire.SessionResponse{}, fmt.Errorf("marshaling request: %w", err)
	}
	reqPath := filepath.Join(t.controlAbs, fmt.Sprintf("%s%d%s", reqPrefix, seq, jsonSuffix))
	if err := atomicWrite(reqPath, body); err != nil {
		return desktopwire.SessionResponse{}, fmt.Errorf("writing request: %w", err)
	}

	respPath := filepath.Join(t.controlAbs, fmt.Sprintf("%s%d%s", respPrefix, seq, jsonSuffix))
	deadline := time.Now().Add(sendTimeout)
	for {
		data, rerr := os.ReadFile(respPath) //nolint:gosec // control path we own
		if rerr == nil {
			var resp desktopwire.SessionResponse
			if err := json.Unmarshal(data, &resp); err != nil {
				return desktopwire.SessionResponse{}, fmt.Errorf("decoding response: %w", err)
			}
			_ = os.Remove(respPath)
			return resp, nil
		}
		if !os.IsNotExist(rerr) {
			return desktopwire.SessionResponse{}, fmt.Errorf("reading response: %w", rerr)
		}
		if e := t.daemonErr(); e != nil {
			return desktopwire.SessionResponse{}, e
		}
		if time.Now().After(deadline) {
			return desktopwire.SessionResponse{}, fmt.Errorf("desktop act timed out after %s", sendTimeout)
		}
		if err := t.sleep(ctx); err != nil {
			return desktopwire.SessionResponse{}, err
		}
	}
}

func (t *fileTransport) close() error {
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _ = t.send(cctx, desktopwire.SessionRequest{Act: desktopwire.Act{Op: desktopwire.OpClose}})
	cancel()

	t.cancel()
	select {
	case <-t.done:
	case <-time.After(5 * time.Second):
	}
	return os.RemoveAll(t.controlAbs)
}

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

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
