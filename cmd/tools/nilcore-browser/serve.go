// Serve (session) mode for nilcore-browser. WHY this exists alongside batch and
// flow: a true browser AGENT observes after each act and decides the next one, so
// the browser must stay alive ACROSS many host turns. NilCore's sandbox runs each
// box.Exec as a fresh, isolated container — there is no persistent process across
// Exec calls — so the session is realized as ONE long-lived `nilcore-browser
// --serve` Exec (launched in a host goroutine) that keeps Chrome up and exchanges
// one Act ⇄ one Observation with the host over a FILE-QUEUE on the shared /work
// mount. The file queue (not a socket) is the portable channel: it works
// identically under the container bind-mount and the namespace shared FS, needs no
// networking (Chrome's debug port stays loopback-internal, I4), and is trivially
// testable. The Observation uses the SAME browserwire contract the host parses.
//
// Everything Chrome returns — titles, text, a11y labels, screenshots — is
// UNTRUSTED data (I7); we transport and decode it, never let it steer control flow.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"nilcore/internal/browserwire"
	"nilcore/internal/cdp"
)

// serve protocol/file names on the shared control dir. The host writes req-<seq>,
// the driver writes resp-<seq>; a "ready" marker tells the host Chrome is up. All
// writes are atomic (tmp + rename) so a reader never sees a partial file.
const (
	readyMarker = "ready"
	reqPrefix   = "req-"
	respPrefix  = "resp-"
	jsonSuffix  = ".json"
)

// serveIdleTimeout shuts a daemon down if the host sends no request for this long
// (a leaked session reaps itself). serveHardTimeout bounds the whole session.
const (
	serveIdleTimeout  = 5 * time.Minute
	serveHardTimeout  = 30 * time.Minute
	servePollInterval = 40 * time.Millisecond
	settleQuietMS     = 120 // DOM-stability gap between WaitReady polls
)

// extractServe pulls `--serve` and `--control <dir>` out of args. Returns serve=false
// (with rest=args) when --serve is absent, so the batch/flow dispatch is unchanged.
func extractServe(args []string) (serve bool, control string, rest []string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--serve":
			serve = true
		case a == "--control":
			if i+1 >= len(args) {
				return false, "", nil, errors.New("--control requires a value")
			}
			control = args[i+1]
			i++
		case strings.HasPrefix(a, "--control="):
			control = strings.TrimPrefix(a, "--control=")
		default:
			rest = append(rest, a)
		}
	}
	if serve && strings.TrimSpace(control) == "" {
		return false, "", nil, errors.New("--serve requires --control <dir>")
	}
	return serve, control, rest, nil
}

// runServe is the daemon: launch Chrome once, connect over CDP, then loop the
// file-queue applying one Act and writing one Observation per request until a
// close act, EOF/idle, or the hard timeout. The live browser path is CI-only;
// the pure pieces (extractServe, applyServeAct dispatch, observation assembly)
// are unit-tested without a browser.
func runServe(ctx context.Context, chromium, control, initialURL string) error {
	if err := os.MkdirAll(control, 0o700); err != nil {
		return fmt.Errorf("creating control dir: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, serveHardTimeout)
	defer cancel()

	port, err := freeLoopbackPort()
	if err != nil {
		return fmt.Errorf("allocating debug port: %w", err)
	}
	userDir, err := os.MkdirTemp("", "nilcore-browser-serve-*")
	if err != nil {
		return fmt.Errorf("creating user-data dir: %w", err)
	}
	defer os.RemoveAll(userDir)

	cmd := exec.CommandContext(runCtx, chromium, interactiveChromiumArgs(port, userDir)...)
	var stderr syncBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launching chromium: %w", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	wsURL, err := waitForDevToolsWS(runCtx, port)
	if err != nil {
		if d := strings.TrimSpace(stderr.String()); d != "" {
			return fmt.Errorf("%w (chromium stderr: %s)", err, lastLine(d))
		}
		return err
	}
	conn, err := cdp.Dial(runCtx, wsURL)
	if err != nil {
		return fmt.Errorf("connecting to devtools: %w", err)
	}
	defer conn.Close()
	if err := conn.Enable(runCtx); err != nil {
		return fmt.Errorf("enabling cdp domains: %w", err)
	}
	if s := strings.TrimSpace(initialURL); s != "" {
		if err := conn.Navigate(runCtx, s); err != nil {
			return err
		}
		_ = conn.WaitReady(runCtx, 2, settleQuietMS)
	}

	// Signal readiness to the host.
	if err := atomicWrite(filepath.Join(control, readyMarker), []byte("1")); err != nil {
		return fmt.Errorf("writing ready marker: %w", err)
	}

	return serveLoop(runCtx, conn, control, &stderr)
}

// serveLoop is the request/response pump: synchronous, one in-flight seq at a time.
func serveLoop(ctx context.Context, conn *cdp.Conn, control string, stderr *syncBuffer) error {
	var version uint64
	seq := 1
	lastReq := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return nil // hard timeout / cancellation is a clean shutdown
		}
		reqPath := filepath.Join(control, fmt.Sprintf("%s%d%s", reqPrefix, seq, jsonSuffix))
		data, err := os.ReadFile(reqPath) //nolint:gosec // control path we created
		if err != nil {
			if os.IsNotExist(err) {
				if time.Since(lastReq) > serveIdleTimeout {
					return nil // idle reap
				}
				if e := sleepShort(ctx); e != nil {
					return nil
				}
				continue
			}
			return fmt.Errorf("reading request: %w", err)
		}
		lastReq = time.Now()

		var req browserwire.SessionRequest
		if err := json.Unmarshal(data, &req); err != nil {
			// A malformed request is reported, not fatal: fail that seq closed.
			_ = writeResp(control, seq, browserwire.SessionResponse{Seq: seq, Error: "bad request json: " + err.Error()})
			_ = os.Remove(reqPath)
			seq++
			continue
		}

		if req.Act.Op == browserwire.OpClose {
			_ = writeResp(control, seq, browserwire.SessionResponse{Seq: seq})
			_ = os.Remove(reqPath)
			return nil
		}

		obs, aerr := applyServeAct(ctx, conn, req.Act, &version, stderr)
		resp := browserwire.SessionResponse{Seq: seq, Observation: obs}
		if aerr != nil {
			resp.Error = aerr.Error()
		}
		if err := writeResp(control, seq, resp); err != nil {
			return fmt.Errorf("writing response: %w", err)
		}
		_ = os.Remove(reqPath)
		seq++
	}
}

// applyServeAct executes one Act and returns the resulting observation. A failed
// act returns the error AND a best-effort observation of the current page, so the
// host can show the model what state the failure left behind.
func applyServeAct(ctx context.Context, conn *cdp.Conn, a browserwire.Act, version *uint64, stderr *syncBuffer) (browserwire.Observation, error) {
	var actErr error
	switch a.Op {
	case browserwire.OpObserve, browserwire.OpExtract:
		// no mutation
	case browserwire.OpNavigate:
		actErr = conn.Navigate(ctx, a.URL)
	case browserwire.OpClick:
		if a.Ref > 0 || a.Selector == "" {
			actErr = conn.ClickRef(ctx, a.Ref)
		} else {
			actErr = conn.ClickSelector(ctx, a.Selector)
		}
	case browserwire.OpType:
		if a.Selector != "" && a.Ref == 0 {
			actErr = conn.TypeIntoSelector(ctx, a.Selector, a.Text)
		} else {
			actErr = conn.TypeRef(ctx, a.Ref, a.Text)
		}
	case browserwire.OpKey:
		actErr = conn.TypeKey(ctx, a.Key)
	case browserwire.OpScroll:
		actErr = scrollAct(ctx, conn, a)
	case browserwire.OpSelect:
		actErr = conn.SelectRef(ctx, a.Ref, a.Text)
	case browserwire.OpBack:
		actErr = conn.Back(ctx)
	case browserwire.OpForward:
		actErr = conn.Forward(ctx)
	case browserwire.OpWait:
		actErr = sleepCtxMS(ctx, a.MS)
	default:
		return browserwire.Observation{}, fmt.Errorf("unsupported act %q", a.Op)
	}

	// Let the page settle, then observe — never act on a still-changing page.
	_ = conn.WaitReady(ctx, 2, settleQuietMS)
	obs := buildServeObservation(ctx, conn, version, stderr)
	return obs, actErr
}

// scrollAct maps a direction+amount to a scrollBy delta. Default amount is one
// viewport-ish step.
func scrollAct(ctx context.Context, conn *cdp.Conn, a browserwire.Act) error {
	amt := a.Amount
	if amt <= 0 {
		amt = 600
	}
	dx, dy := 0, 0
	switch strings.ToLower(a.Dir) {
	case "up":
		dy = -amt
	case "left":
		dx = -amt
	case "right":
		dx = amt
	default: // down
		dy = amt
	}
	return conn.Scroll(ctx, dx, dy)
}

// buildServeObservation snapshots the current page into the browserwire contract:
// the a11y set-of-marks is the primary perception; a screenshot is captured ONLY
// when there are no interactive refs (a canvas/WebGL fallback) to keep token cost
// down. It bumps *version so the host can track ref staleness.
func buildServeObservation(ctx context.Context, conn *cdp.Conn, version *uint64, stderr *syncBuffer) browserwire.Observation {
	*version++
	obs := browserwire.Observation{Version: *version}
	obs.Title, _ = conn.Title(ctx)
	obs.Title = collapseWS(obs.Title)
	obs.URL, _ = conn.CurrentURL(ctx)
	if t, err := conn.Text(ctx); err == nil {
		t = collapseWS(t)
		if len(t) > maxText {
			t = t[:maxText]
		}
		obs.Text = t
	}
	els, _ := conn.InteractiveSnapshot(ctx)
	for _, e := range els {
		// Stamp each ref with this snapshot's version so the host-side Session.Act can
		// reject a ref carried over from an earlier snapshot even when a re-render reused
		// the same positional id (the Cancel→Delete defense — not membership-only).
		obs.Refs = append(obs.Refs, browserwire.Ref{ID: e.Ref, Role: e.Role, Name: e.Name, Value: e.Value, Version: *version})
	}
	if len(obs.Refs) == 0 {
		// No structured affordances → fall back to a screenshot so the model can
		// still perceive a canvas/WebGL/native-rendered page.
		if shot, err := conn.Screenshot(ctx); err == nil {
			obs.ScreenshotB64 = shot
		}
	}
	obs.Tabs = []browserwire.Tab{{ID: "main", Title: obs.Title, URL: obs.URL, Active: true}}
	if stderr != nil {
		obs.Console = collectConsole(stderr.String())
	}
	return obs
}

// ───────────────────────────── file-queue helpers ─────────────────────────────

func writeResp(control string, seq int, resp browserwire.SessionResponse) error {
	b, err := marshalNoHTMLEscape(resp)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(control, fmt.Sprintf("%s%d%s", respPrefix, seq, jsonSuffix)), b)
}

// atomicWrite writes data to a temp file in the same dir and renames it into place,
// so a concurrent reader never observes a partial write.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func marshalNoHTMLEscape(v any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(b.String(), "\n")), nil
}

func sleepShort(ctx context.Context) error {
	t := time.NewTimer(servePollInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func sleepCtxMS(ctx context.Context, ms int) error {
	if ms <= 0 {
		return nil
	}
	if ms > maxWaitMS {
		ms = maxWaitMS
	}
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
