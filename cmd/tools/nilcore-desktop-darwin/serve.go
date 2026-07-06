package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"nilcore/internal/desktop"
	"nilcore/internal/desktopwire"
	"nilcore/internal/som"
)

// This file is CU-MAC-T01: the serve loop (the SAME desktopwire file-queue protocol
// as the Linux driver) plus the macOS observe/perform. The MVP runs the SoM ladder
// at Rungs 2/3 only (no AXUIElement → no Rung 1; that is the production signed
// helper, CU-MAC-T05). Perception = a screencapture-ed, CV-marked or raw screenshot;
// actuation = cliclick at a true macOS POINT (coords.go does resized→pixel→point).

const (
	readyMarker      = "ready"
	reqPrefix        = "req-"
	respPrefix       = "resp-"
	jsonSuffix       = ".json"
	pollInterval     = 40 * time.Millisecond
	serveIdleTimeout = 5 * time.Minute
	serveHardTimeout = 30 * time.Minute
	settleMS         = 150
	imgMaxW          = 1280
	imgMaxH          = 800
)

type driver struct {
	native bool
	idBox  map[int]image.Rectangle // ref id → PIXEL box (from CV detect)
	scaleX float64                 // orig-pixel / resized (x)
	scaleY float64                 // orig-pixel / resized (y)
	bscale float64                 // backing scale (pixel / point)
	origX  int                     // display origin (points) — primary = 0 in the MVP
	origY  int
	ver    uint64
}

func runServe(ctx context.Context, control string, native bool) error {
	if err := os.MkdirAll(control, 0o700); err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, serveHardTimeout)
	defer cancel()

	d := &driver{native: native, idBox: map[int]image.Rectangle{}, scaleX: 1, scaleY: 1, bscale: fallbackBackingScale}

	// Host-mode honesty (ROADMAP-COMPUTER-USE-DARWIN §3): surface the recorded
	// host-control limitations once at startup — before any synthetic input runs — so
	// the operator knows the screenshot terminal-exclusion is fail-closed-only and that
	// synthetic CGEvents are not yet source-userData-tagged (both need the signed helper).
	for _, n := range hostModeNotes() {
		fmt.Fprintln(os.Stderr, "nilcore-desktop-darwin [host-mode]:", n)
	}

	if err := atomicWrite(filepath.Join(control, readyMarker), []byte("1")); err != nil {
		return err
	}

	seq := 1
	lastReq := time.Now()
	for {
		if runCtx.Err() != nil {
			return nil
		}
		reqPath := filepath.Join(control, reqPrefix+strconv.Itoa(seq)+jsonSuffix)
		data, err := os.ReadFile(reqPath) //nolint:gosec // control path we created
		if err != nil {
			if os.IsNotExist(err) {
				if time.Since(lastReq) > serveIdleTimeout {
					return nil
				}
				if e := sleepCtx(runCtx, int(pollInterval.Milliseconds())); e != nil {
					return nil
				}
				continue
			}
			return err
		}
		lastReq = time.Now()

		var req desktopwire.SessionRequest
		if jerr := json.Unmarshal(data, &req); jerr != nil {
			_ = writeResp(control, seq, desktopwire.SessionResponse{Seq: seq, Error: "bad request json: " + jerr.Error()})
			_ = shredFile(reqPath)
			seq++
			continue
		}
		if req.Act.Op == desktopwire.OpClose {
			_ = writeResp(control, seq, desktopwire.SessionResponse{Seq: seq})
			_ = shredFile(reqPath)
			return nil
		}

		obs, aerr := d.apply(runCtx, req.Act)
		resp := desktopwire.SessionResponse{Seq: seq, Observation: obs}
		if aerr != nil {
			resp.Error = aerr.Error()
		}
		if werr := writeResp(control, seq, resp); werr != nil {
			return werr
		}
		_ = shredFile(reqPath)
		seq++
	}
}

func (d *driver) apply(ctx context.Context, a desktopwire.Act) (desktopwire.Observation, error) {
	actErr := d.perform(ctx, a)
	if a.Op != desktopwire.OpObserve {
		_ = sleepCtx(ctx, settleMS)
	}
	return d.observe(ctx), actErr
}

func (d *driver) perform(ctx context.Context, a desktopwire.Act) error {
	// Host-control hardening (CU-MAC-T09): the kill-switch + per-app allowlist gate
	// every MUTATING act, fail-closed. Observe/wait never touch the desktop.
	if a.Op != desktopwire.OpObserve && a.Op != desktopwire.OpWait {
		if err := guardMutation(ctx); err != nil {
			return err
		}
	}
	switch a.Op {
	case desktopwire.OpObserve:
		return nil
	case desktopwire.OpWait:
		return sleepCtx(ctx, a.MS)
	case desktopwire.OpKey:
		if a.Key == "" {
			return fmt.Errorf("key requires a chord")
		}
		return runCliclick(ctx, cliclickKey(a.Key))
	case desktopwire.OpScroll:
		cmd, err := cliclickScroll(a.Dir, a.Amount)
		if err != nil {
			return err
		}
		return runCliclick(ctx, cmd)
	case desktopwire.OpClick:
		x, y, err := d.resolvePoint(a)
		if err != nil {
			return err
		}
		cmd, err := cliclickClickN(x, y, a.Button, a.Count)
		if err != nil {
			return err
		}
		return runCliclick(ctx, cmd)
	case desktopwire.OpDrag:
		x0, y0, err := d.resolvePoint(a)
		if err != nil {
			return err
		}
		x1, y1, err := d.resolveTo(a)
		if err != nil {
			return err
		}
		cmd, err := cliclickDrag(x0, y0, x1, y1, a.Button)
		if err != nil {
			return err
		}
		return runCliclick(ctx, cmd)
	case desktopwire.OpMouseDown:
		x, y, err := d.resolvePoint(a)
		if err != nil {
			return err
		}
		cmd, err := cliclickMouseDown(x, y, a.Button)
		if err != nil {
			return err
		}
		return runCliclick(ctx, cmd)
	case desktopwire.OpMouseUp:
		x, y, err := d.resolvePoint(a)
		if err != nil {
			return err
		}
		cmd, err := cliclickMouseUp(x, y, a.Button)
		if err != nil {
			return err
		}
		return runCliclick(ctx, cmd)
	case desktopwire.OpType:
		if a.Ref > 0 {
			if x, y, err := d.resolvePoint(a); err == nil {
				_ = runCliclick(ctx, cliclickClick(x, y))
			}
		}
		return runCliclick(ctx, cliclickType(a.Text))
	default:
		return fmt.Errorf("unsupported act %q", a.Op)
	}
}

// resolvePoint maps an act to a true macOS POINT: a ref via its pixel-box centre, or
// a coordinate via the resized→pixel→point map.
func (d *driver) resolvePoint(a desktopwire.Act) (int, int, error) {
	if a.Ref > 0 {
		b, ok := d.idBox[a.Ref]
		if !ok {
			return 0, 0, fmt.Errorf("ref %d is not in the current snapshot (re-observe)", a.Ref)
		}
		x, y := pixelCenterToPoint(b, d.bscale, d.origX, d.origY)
		return x, y, nil
	}
	if len(a.Coordinate) == 2 {
		x, y := resizedToPoint(a.Coordinate[0], a.Coordinate[1], d.scaleX, d.scaleY, d.bscale, d.origX, d.origY)
		return x, y, nil
	}
	return 0, 0, fmt.Errorf("click needs a ref or a coordinate")
}

// resolveTo maps a drag DESTINATION (Act.To, in resized image space) to a true macOS
// POINT. A drag without a destination fails closed rather than dragging to (0,0).
func (d *driver) resolveTo(a desktopwire.Act) (int, int, error) {
	if len(a.To) != 2 {
		return 0, 0, fmt.Errorf("drag needs a destination (to:[x,y])")
	}
	x, y := resizedToPoint(a.To[0], a.To[1], d.scaleX, d.scaleY, d.bscale, d.origX, d.origY)
	return x, y, nil
}

// observe captures the real desktop and builds a Rung-2 (SoM-marked) or Rung-3 (raw)
// observation. The MVP has no AXUIElement, so there is no Rung 1; the SoM box source
// is the pure-Go classical-CV detector (internal/desktop.Detect) over the screenshot.
func (d *driver) observe(ctx context.Context) desktopwire.Observation {
	d.ver++
	d.idBox = map[int]image.Rectangle{}
	obs := desktopwire.Observation{Version: d.ver}

	// Controlling-terminal exclusion (ROADMAP §0.3/§3): screencapture grabs the whole
	// display and cannot subtract a window, so when the agent's own terminal is
	// frontmost — where the human-gate prompt and any secrets are rendered — we REFUSE
	// to capture rather than feed those pixels to the model (I3/I7). Fail closed.
	if terminalIsFrontmost(ctx) {
		obs.Rung = desktopwire.RungCoordinate
		obs.Console = []string{"capture refused: the controlling terminal (" + controllingTerminal() + ") is frontmost — its contents (approval prompt / secrets) must not be sent to the model. Bring the target app forward and re-observe."}
		return obs
	}

	full, err := capture(ctx)
	if err != nil || full == nil {
		obs.Rung = desktopwire.RungCoordinate
		obs.Console = []string{"capture failed: " + errStr(err)}
		return obs
	}
	scale, determined := resolveBackingScale(ctx, full.Bounds().Dx())
	d.bscale = scale
	if !determined {
		// The backing scale could not be determined (Automation TCC denied / Finder
		// bounds unreadable) and NILCORE_MAC_SCALE is unset. We fell back to unity so a
		// 1× display maps correctly; a Retina display will mis-click until the operator
		// sets the deterministic override. Surface it so the failure is never silent.
		obs.Console = append(obs.Console, "backing scale undetermined (no NILCORE_MAC_SCALE, osascript desktop-width probe failed) — assuming 1.0; set NILCORE_MAC_SCALE on a Retina display to avoid mis-clicks")
	}
	display, sx, sy := resizeNearest(full, imgMaxW, imgMaxH)
	d.scaleX, d.scaleY = sx, sy

	if d.native {
		// Path A: raw screenshot, coordinate mode (no marks).
		obs.Rung = desktopwire.RungCoordinate
		obs.ScreenshotB64 = pngB64(display)
		return obs
	}

	cvBoxes := desktop.Detect(full, desktop.DefaultDetectOptions())
	if len(cvBoxes) == 0 {
		obs.Rung = desktopwire.RungCoordinate
		obs.ScreenshotB64 = pngB64(display)
		return obs
	}

	// Rung 2: mark the resized screenshot; refs are the numbered boxes; idBox keeps
	// the PIXEL boxes for clicking (converted to points at action time).
	marks := make([]som.Mark, 0, len(cvBoxes))
	id := 1
	for _, r := range cvBoxes {
		marks = append(marks, som.Mark{ID: id, Box: scaleRectToResized(r, sx, sy), Role: "element"})
		d.idBox[id] = r
		// Stamp each ref with this observation's version so the host-side stale-ref guard
		// (desktopsession.validateRef) fails closed on a ref whose positional id was reused
		// by a later re-render — mirrors the browser tier's version-reject.
		obs.Refs = append(obs.Refs, desktopwire.Ref{ID: id, Role: "element", Version: d.ver,
			Box: desktopwire.Box{X: r.Min.X, Y: r.Min.Y, W: r.Dx(), H: r.Dy()}})
		id++
	}
	marked, _ := som.Overlay(display, marks)
	obs.Rung = desktopwire.RungSoM
	obs.ScreenshotB64 = pngB64(marked)
	return obs
}

// scaleRectToResized maps a pixel rect into the resized image space for the overlay.
func scaleRectToResized(r image.Rectangle, scaleX, scaleY float64) image.Rectangle {
	return image.Rect(
		int(float64(r.Min.X)/scaleX), int(float64(r.Min.Y)/scaleY),
		int(float64(r.Max.X)/scaleX), int(float64(r.Max.Y)/scaleY),
	)
}

// ── file-queue helpers ──

func writeResp(control string, seq int, resp desktopwire.SessionResponse) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(control, respPrefix+strconv.Itoa(seq)+jsonSuffix), b)
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// shredFile best-effort overwrites a request file's bytes with zeros before removing it.
//
// The host-control transport (internal/desktopsession/hosttransport.go) writes each Act —
// which may carry a host-side-resolved {{secret:NAME}} value — as plaintext JSON to a real
// /tmp file, so I3 ("secrets never on-disk plaintext") is DELIBERATELY STRESSED on this
// tier. That is a recorded, accepted residual of the native-macOS host-control relaxation
// (docs/ROADMAP-COMPUTER-USE-DARWIN.md §0/§3 — the tier that drives the operator's real
// desktop behind the louder host gate). Shredding is cheap defense-in-depth, NOT a fix:
// zeroing the bytes before unlink means a crash between processing and removal leaves less
// secret material recoverable on disk. It is best-effort — a fsync-less overwrite on a
// journaling/COW filesystem is not a guarantee — so failures are ignored and we still
// remove the file.
func shredFile(path string) error {
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
		if f, oerr := os.OpenFile(path, os.O_WRONLY, 0o600); oerr == nil { //nolint:gosec // control path we created
			zeros := make([]byte, fi.Size())
			_, _ = f.WriteAt(zeros, 0)
			_ = f.Sync()
			_ = f.Close()
		}
	}
	return os.Remove(path)
}

func sleepCtx(ctx context.Context, ms int) error {
	if ms <= 0 {
		return nil
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

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
