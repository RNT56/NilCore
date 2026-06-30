package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"nilcore/internal/desktop"
	"nilcore/internal/desktopwire"
	"nilcore/internal/som"
)

func errf(format string, a ...any) error { return fmt.Errorf(format, a...) }

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

const (
	readyMarker      = "ready"
	reqPrefix        = "req-"
	respPrefix       = "resp-"
	jsonSuffix       = ".json"
	pollInterval     = 40 * time.Millisecond
	serveIdleTimeout = 5 * time.Minute
	serveHardTimeout = 30 * time.Minute
	settleMS         = 150
	// imgMaxW/H bound the screenshot sent to the model (the pixel budget). XGA-ish.
	imgMaxW = 1280
	imgMaxH = 800
)

// driver holds the session state across the serve loop: the rung ladder, the
// current id→true-pixel-box map (for ref clicks), the last resize scale (for Rung-3
// coordinate rescale), the snapshot version, and the last signature (for stagnation).
type driver struct {
	native  bool
	ladder  *desktop.Ladder
	idBox   map[int]desktopwire.Box
	scaleX  float64
	scaleY  float64
	ver     uint64
	lastSig string
}

// runServe is the daemon: write the ready marker, then pump the file-queue applying
// one act and returning one observation per request until a close act / idle / hard
// timeout. The live X11 path is CI-only; the assembly logic is unit-tested.
func runServe(ctx context.Context, control string, native bool) error {
	if err := os.MkdirAll(control, 0o700); err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, serveHardTimeout)
	defer cancel()

	d := &driver{native: native, ladder: desktop.NewLadder(), idBox: map[int]desktopwire.Box{}, scaleX: 1, scaleY: 1}

	// Bring up the contained X11 desktop (Xvfb + WM + apps) before serving. Live, CI-
	// only; a var so unit tests of runServe never reach it. A failure is fatal — the
	// driver fails closed rather than serving over a dead display.
	if err := ensureDisplay(runCtx); err != nil {
		return err
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
				if e := sleepCtxDur(runCtx, pollInterval); e != nil {
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

// apply performs one act (if mutating), then re-observes via the rung ladder.
func (d *driver) apply(ctx context.Context, a desktopwire.Act) (desktopwire.Observation, error) {
	preSig := d.lastSig
	actErr := d.perform(ctx, a)
	if a.Op != desktopwire.OpObserve {
		_ = sleepCtx(ctx, settleMS) // let the screen settle before observing
	}
	obs := d.observe(ctx, a, preSig)
	return obs, actErr
}

// perform translates a mutating act into an xdotool command (or a wait). observe is
// not a mutation. A ref click resolves through the id→true-box map; a coordinate
// click rescales resized→true; both fail closed on a missing target.
func (d *driver) perform(ctx context.Context, a desktopwire.Act) error {
	switch a.Op {
	case desktopwire.OpObserve:
		return nil
	case desktopwire.OpWait:
		return sleepCtx(ctx, a.MS)
	case desktopwire.OpKey:
		if strings.TrimSpace(a.Key) == "" {
			return errf("key requires a chord")
		}
		return runXdotool(ctx, xdotoolKey(a.Key))
	case desktopwire.OpScroll:
		return runXdotool(ctx, xdotoolScroll(a.Dir, a.Amount))
	case desktopwire.OpClick:
		x, y, err := d.resolvePoint(a)
		if err != nil {
			return err
		}
		return runXdotool(ctx, xdotoolClick(x, y))
	case desktopwire.OpType:
		if a.Ref > 0 { // focus the field first
			if x, y, err := d.resolvePoint(a); err == nil {
				_ = runXdotool(ctx, xdotoolClick(x, y))
			}
		}
		return runXdotool(ctx, xdotoolType(a.Text))
	default:
		return errf("unsupported act %q", a.Op)
	}
}

// resolvePoint maps an act to a true-screen (x,y): a ref via the id→true-box centre,
// or a coordinate via the resized→true rescale.
func (d *driver) resolvePoint(a desktopwire.Act) (int, int, error) {
	if a.Ref > 0 {
		b, ok := d.idBox[a.Ref]
		if !ok {
			return 0, 0, errf("ref %d is not in the current snapshot (re-observe)", a.Ref)
		}
		x, y := b.Center()
		return x, y, nil
	}
	if len(a.Coordinate) == 2 {
		x, y := rescaleCoord(a.Coordinate[0], a.Coordinate[1], d.scaleX, d.scaleY)
		return x, y, nil
	}
	return 0, 0, errf("click needs a ref or a coordinate")
}

// observe runs the Set-of-Marks ladder and builds the observation. Rung 1 returns
// AT-SPI refs (no image); Rung 2 a SoM-marked screenshot whose refs are the numbered
// boxes; Rung 3 a raw screenshot (no refs, coordinate mode).
func (d *driver) observe(ctx context.Context, a desktopwire.Act, preSig string) desktopwire.Observation {
	if d.native {
		return d.observeNative(ctx)
	}
	d.ver++
	window := activeWindow(ctx)
	a11y, _ := parseA11y(mustDump(ctx))

	// Stagnation drives the 1→2 ladder downgrade. It is NOT ref-click-only: ANY mutating
	// op (click/type/key/scroll, ref- or coordinate-based) whose post-act signature
	// equals the pre-act one verifiably changed nothing — a tree that lies for typing
	// must drop us off Rung 1 just like a lying ref-click does (mirrors desktopagent
	// .isStagnant's "any non-observe op" scope, B7-cu.5).
	postSig := sigOf(window, len(a11y))
	stagnant := a.Op != desktopwire.OpObserve && a.Op != desktopwire.OpWait && postSig == preSig

	obs := desktopwire.Observation{Version: d.ver, FocusedWindow: collapse(window), Title: collapse(window)}
	d.idBox = map[int]desktopwire.Box{}

	// First, decide whether Rung 1 (AT-SPI refs) is still trusted for this window. We
	// pass HasMarkableBoxes=true here only to let Decide return Rung 1 vs "needs a
	// screenshot"; the real 2-vs-3 choice is made below from the actual mark count.
	if dec := d.ladder.Decide(desktop.RungInput{Window: window, RefCount: len(a11y), Stagnant: stagnant, HasMarkableBoxes: true}); dec.Rung == desktopwire.RungATSPI {
		obs.Rung = desktopwire.RungATSPI
		obs.Refs = a11y
		for _, r := range a11y {
			d.idBox[r.ID] = r.Box
		}
		d.lastSig = postSig
		return obs
	}

	// Rung 2/3 need a screenshot.
	full, cerr := capture(ctx)
	if cerr != nil || full == nil {
		// No capture and (per the ladder) no usable refs — return what little we have.
		// Drive the ladder's 2→3 transition explicitly with the real (zero) markable-box
		// availability, so the per-window cache reflects reality (B7-cu.4).
		d.ladder.Decide(desktop.RungInput{Window: window, RefCount: len(a11y), Stagnant: stagnant, HasMarkableBoxes: false})
		obs.Rung = desktopwire.RungCoordinate
		obs.Console = []string{"capture failed: " + errStr(cerr)}
		d.lastSig = sigOf(window, 0)
		return obs
	}

	marks, marksBox := d.buildMarks(full, a11y)
	display, sx, sy := resizeNearest(full, imgMaxW, imgMaxH)
	d.scaleX, d.scaleY = sx, sy

	// Now that we know whether ANY markable box exists, drive the ladder's 2→3 decision
	// from the truth instead of a hardcoded true: an empty mark set is the documented
	// "neither AT-SPI nor CV yields a plausible mark" Rung-3 trigger (B7-cu.4).
	dec := d.ladder.Decide(desktop.RungInput{Window: window, RefCount: len(a11y), Stagnant: stagnant, HasMarkableBoxes: len(marks) > 0})

	if dec.Rung == desktopwire.RungCoordinate || len(marks) == 0 {
		// Rung 3: raw screenshot, coordinate mode.
		obs.Rung = desktopwire.RungCoordinate
		obs.ScreenshotB64 = pngB64(display)
		d.lastSig = sigOf(window, 0)
		return obs
	}

	// Rung 2: overlay the marks (scaled into the resized image) and return them as refs.
	somMarks := make([]som.Mark, 0, len(marks))
	for _, m := range marks {
		somMarks = append(somMarks, som.Mark{ID: m.ID, Box: scaleBoxToResized(marksBox[m.ID], sx, sy), Role: m.Role, Label: m.Name})
	}
	marked, _ := som.Overlay(display, somMarks)
	obs.Rung = desktopwire.RungSoM
	obs.ScreenshotB64 = pngB64(marked)
	obs.Refs = marks
	for id, b := range marksBox {
		d.idBox[id] = b // TRUE-pixel boxes for clicking
	}
	d.lastSig = sigOf(window, len(marks))
	return obs
}

// buildMarks combines AT-SPI extents (with role/name) and classical-CV proposals
// into a numbered mark list (refs) plus the id→true-box map. AT-SPI boxes come first
// (they carry semantics); CV boxes fill the rest, capped by detect's own cap.
func (d *driver) buildMarks(full image.Image, a11y []desktopwire.Ref) ([]desktopwire.Ref, map[int]desktopwire.Box) {
	refs := make([]desktopwire.Ref, 0, len(a11y))
	boxes := map[int]desktopwire.Box{}
	id := 1
	for _, r := range a11y {
		nr := r
		nr.ID = id
		refs = append(refs, nr)
		boxes[id] = r.Box
		id++
	}
	for _, rect := range desktop.Detect(full, desktop.DefaultDetectOptions()) {
		b := desktopwire.Box{X: rect.Min.X, Y: rect.Min.Y, W: rect.Dx(), H: rect.Dy()}
		refs = append(refs, desktopwire.Ref{ID: id, Role: "element", Box: b})
		boxes[id] = b
		id++
	}
	return refs, boxes
}

// ── helpers ──

func mustDump(ctx context.Context) string {
	s, err := dumpA11y(ctx)
	if err != nil {
		return "[]"
	}
	return s
}

func sigOf(window string, refCount int) string {
	return window + "|" + strconv.Itoa(refCount)
}

func pngB64(img image.Image) string {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func collapse(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\n", " ")) }

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

func sleepCtxDur(ctx context.Context, d time.Duration) error {
	return sleepCtx(ctx, int(d.Milliseconds()))
}
