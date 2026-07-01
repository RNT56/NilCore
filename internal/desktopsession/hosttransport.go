package desktopsession

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"nilcore/internal/desktopwire"
)

// hostTransport drives a desktop driver running as a HOST subprocess (NOT a
// sandboxed box.Exec) over a file-queue in a host temp dir. It is the transport for
// native-macOS host-control mode (CU-MAC-T09 MVP): there is no container/guest, so
// the driver (nilcore-desktop-darwin) runs directly on the host and the same
// file-queue protocol carries acts/observations. This deliberately has NO sandbox
// boundary — I4 is relaxed; it is reached only behind the louder host gate.
type hostTransport struct {
	controlAbs string
	seq        int

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	procE  error
}

// newHostTransport launches the driver binary as a host subprocess serving over the
// given control dir. A missing/failed binary surfaces via send/waitReady (fail closed).
func newHostTransport(_ context.Context, driver, control string, native bool) (*hostTransport, error) {
	if err := os.MkdirAll(control, 0o700); err != nil {
		return nil, fmt.Errorf("creating control dir: %w", err)
	}
	args := []string{"--serve", "--control", control}
	if native {
		args = append(args, "--native")
	}
	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, driver, args...) //nolint:gosec // operator-trusted driver path
	ht := &hostTransport{controlAbs: control, cancel: cancel, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("launching %s: %w", driver, err)
	}
	go func() {
		defer close(ht.done)
		err := cmd.Wait()
		ht.mu.Lock()
		if err != nil {
			ht.procE = fmt.Errorf("desktop driver exited: %w", err)
		} else {
			ht.procE = fmt.Errorf("desktop driver exited")
		}
		ht.mu.Unlock()
	}()
	return ht, nil
}

func (t *hostTransport) procErr() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.procE
}

func (t *hostTransport) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(readyTimeout)
	marker := filepath.Join(t.controlAbs, readyMarker)
	for {
		if _, err := os.Stat(marker); err == nil {
			return nil
		}
		if e := t.procErr(); e != nil {
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

func (t *hostTransport) send(ctx context.Context, req desktopwire.SessionRequest) (desktopwire.SessionResponse, error) {
	t.seq++
	seq := t.seq
	req.Seq = seq
	body, err := json.Marshal(req)
	if err != nil {
		return desktopwire.SessionResponse{}, err
	}
	// I3 relaxation (RECORDED): the Act's JSON may carry a host-side-resolved
	// {{secret:NAME}} value, and on this host-control tier it is written to a real /tmp
	// file (no /work bind-mount, no sandbox). This is the accepted residual documented in
	// docs/ROADMAP-COMPUTER-USE-DARWIN.md §0/§3 — the tier reached only behind the louder
	// host gate. Defense-in-depth: the driver shredFile()s (zero-then-unlink) this request
	// after processing, so a crash leaves less secret material recoverable on disk.
	reqPath := filepath.Join(t.controlAbs, fmt.Sprintf("%s%d%s", reqPrefix, seq, jsonSuffix))
	if err := atomicWrite(reqPath, body); err != nil {
		return desktopwire.SessionResponse{}, err
	}
	respPath := filepath.Join(t.controlAbs, fmt.Sprintf("%s%d%s", respPrefix, seq, jsonSuffix))
	deadline := time.Now().Add(sendTimeout)
	for {
		data, rerr := os.ReadFile(respPath) //nolint:gosec // control path we own
		if rerr == nil {
			var resp desktopwire.SessionResponse
			if err := json.Unmarshal(data, &resp); err != nil {
				return desktopwire.SessionResponse{}, err
			}
			_ = os.Remove(respPath)
			return resp, nil
		}
		if !os.IsNotExist(rerr) {
			return desktopwire.SessionResponse{}, rerr
		}
		if e := t.procErr(); e != nil {
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

func (t *hostTransport) close() error {
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

func (t *hostTransport) sleep(ctx context.Context) error {
	tm := time.NewTimer(pollInterval)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tm.C:
		return nil
	}
}

// HostOptions configure a host-control desktop session.
type HostOptions struct {
	Driver  string // path/name of the host desktop driver (default "nilcore-desktop-darwin")
	Native  bool
	Secrets SecretResolver
}

// LaunchHost starts a desktop session driven by a HOST subprocess (no sandbox) — the
// native-macOS host-control MVP. The Session's ref guard + {{secret}} substitution
// are reused unchanged; only the transport differs (no /work bind-mount). The caller
// is responsible for the louder host-control gate before calling this.
func LaunchHost(ctx context.Context, opt HostOptions) (*Session, desktopwire.Observation, error) {
	id, err := newID()
	if err != nil {
		return nil, desktopwire.Observation{}, err
	}
	control := filepath.Join(os.TempDir(), "nilcore-desktop-host-"+id)
	driver := opt.Driver
	if driver == "" {
		driver = "nilcore-desktop-darwin"
	}
	tr, err := newHostTransport(ctx, driver, control, opt.Native)
	if err != nil {
		return nil, desktopwire.Observation{}, err
	}
	s := &Session{tr: tr, secrets: opt.Secrets}
	if err := tr.waitReady(ctx); err != nil {
		_ = tr.close()
		return nil, desktopwire.Observation{}, fmt.Errorf("desktop driver never became ready: %w", err)
	}
	obs, err := s.Observe(ctx)
	if err != nil {
		_ = s.Close()
		return nil, desktopwire.Observation{}, err
	}
	return s, obs, nil
}
