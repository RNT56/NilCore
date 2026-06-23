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

	d := &driver{native: native, idBox: map[int]image.Rectangle{}, scaleX: 1, scaleY: 1, bscale: 2}

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
			_ = os.Remove(reqPath)
			seq++
			continue
		}
		if req.Act.Op == desktopwire.OpClose {
			_ = writeResp(control, seq, desktopwire.SessionResponse{Seq: seq})
			_ = os.Remove(reqPath)
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
		_ = os.Remove(reqPath)
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
		return runCliclick(ctx, cliclickScroll(a.Dir, a.Amount))
	case desktopwire.OpClick:
		x, y, err := d.resolvePoint(a)
		if err != nil {
			return err
		}
		return runCliclick(ctx, cliclickClick(x, y))
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

// observe captures the real desktop and builds a Rung-2 (SoM-marked) or Rung-3 (raw)
// observation. The MVP has no AXUIElement, so there is no Rung 1; the SoM box source
// is the pure-Go classical-CV detector (internal/desktop.Detect) over the screenshot.
func (d *driver) observe(ctx context.Context) desktopwire.Observation {
	d.ver++
	d.idBox = map[int]image.Rectangle{}
	obs := desktopwire.Observation{Version: d.ver}

	full, err := capture(ctx)
	if err != nil || full == nil {
		obs.Rung = desktopwire.RungCoordinate
		obs.Console = []string{"capture failed: " + errStr(err)}
		return obs
	}
	d.bscale = backingScale(ctx, full.Bounds().Dx())
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
		obs.Refs = append(obs.Refs, desktopwire.Ref{ID: id, Role: "element",
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
