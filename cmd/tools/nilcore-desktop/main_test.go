package main

import (
	"context"
	"image"
	"image/color"
	"reflect"
	"testing"

	"nilcore/internal/desktop"
	"nilcore/internal/desktopwire"
)

func TestParseArgs(t *testing.T) {
	serve, control, native, err := parseArgs([]string{"--serve", "--control", ".nilcore/desktop/x", "--native"})
	if err != nil || !serve || control != ".nilcore/desktop/x" || !native {
		t.Fatalf("parse = (%v,%q,%v,%v)", serve, control, native, err)
	}
	if _, _, _, err := parseArgs([]string{"--serve"}); err == nil {
		t.Fatal("--serve without --control must error")
	}
	if _, _, _, err := parseArgs([]string{"--bogus"}); err == nil {
		t.Fatal("unknown flag must error")
	}
}

func TestParseA11y(t *testing.T) {
	js := `[{"role":"push button","name":"Save","box":{"x":10,"y":20,"w":40,"h":15},"actions":["click"]},
	        {"role":"label","name":"zero","box":{"x":0,"y":0,"w":0,"h":0}},
	        {"role":"entry","name":"Email","box":{"x":5,"y":5,"w":100,"h":24}}]`
	refs, err := parseA11y(js)
	if err != nil {
		t.Fatal(err)
	}
	// The zero-box node is dropped; ids are sequential over the kept nodes.
	if len(refs) != 2 {
		t.Fatalf("kept %d refs, want 2: %+v", len(refs), refs)
	}
	if refs[0].ID != 1 || refs[0].Name != "Save" || refs[0].Box.W != 40 || refs[0].Actions[0] != "click" {
		t.Fatalf("ref0 = %+v", refs[0])
	}
	if refs[1].ID != 2 || refs[1].Role != "entry" {
		t.Fatalf("ref1 = %+v", refs[1])
	}
}

func TestXdotoolBuilders(t *testing.T) {
	if got := xdotoolClick(12, 34); !reflect.DeepEqual(got, []string{"mousemove", "--sync", "12", "34", "click", "1"}) {
		t.Fatalf("click args = %v", got)
	}
	if got := xdotoolType("a-b"); !reflect.DeepEqual(got, []string{"type", "--clearmodifiers", "--", "a-b"}) {
		t.Fatalf("type args = %v", got)
	}
	if got := xdotoolKey("ctrl+s"); !reflect.DeepEqual(got, []string{"key", "--clearmodifiers", "ctrl+s"}) {
		t.Fatalf("key args = %v", got)
	}
	if got := xdotoolScroll("up", 5); !reflect.DeepEqual(got, []string{"click", "--repeat", "5", "4"}) {
		t.Fatalf("scroll args = %v", got)
	}
}

func TestRescaleAndResize(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1000, 500))
	dst, sx, sy := resizeNearest(src, 100, 100)
	if dst.Bounds().Dx() != 100 || dst.Bounds().Dy() != 50 { // aspect preserved
		t.Fatalf("resized to %v, want 100x50", dst.Bounds())
	}
	if sx != 10 || sy != 10 {
		t.Fatalf("scale = (%v,%v), want (10,10)", sx, sy)
	}
	// A coordinate in resized space maps back ×scale.
	x, y := rescaleCoord(50, 25, sx, sy)
	if x != 500 || y != 250 {
		t.Fatalf("rescaled = (%d,%d), want (500,250)", x, y)
	}
}

// withSeams swaps the live X11 seams for fakes and restores them.
func withSeams(t *testing.T, a11y string, img image.Image, window string, rec *[][]string) func() {
	t.Helper()
	od, oc, ow, ox := dumpA11y, capture, activeWindow, runXdotool
	dumpA11y = func(context.Context) (string, error) { return a11y, nil }
	capture = func(context.Context) (image.Image, error) { return img, nil }
	activeWindow = func(context.Context) string { return window }
	runXdotool = func(_ context.Context, args []string) error {
		if rec != nil {
			*rec = append(*rec, args)
		}
		return nil
	}
	return func() { dumpA11y, capture, activeWindow, runXdotool = od, oc, ow, ox }
}

func newDriver() *driver {
	return &driver{ladder: desktop.NewLadder(), idBox: map[int]desktopwire.Box{}, scaleX: 1, scaleY: 1}
}

func TestObserveRung1_ATSPI(t *testing.T) {
	restore := withSeams(t, `[{"role":"push button","name":"OK","box":{"x":10,"y":10,"w":50,"h":20},"actions":["click"]}]`, nil, "Settings", nil)
	defer restore()
	d := newDriver()
	obs := d.observe(context.Background(), desktopwire.Act{Op: desktopwire.OpObserve}, "")
	if obs.Rung != desktopwire.RungATSPI {
		t.Fatalf("rung = %d, want 1", obs.Rung)
	}
	if len(obs.Refs) != 1 || obs.Refs[0].Name != "OK" {
		t.Fatalf("refs = %+v", obs.Refs)
	}
	if obs.ScreenshotB64 != "" {
		t.Fatal("rung 1 should carry no screenshot")
	}
	if d.idBox[1].W != 50 {
		t.Fatalf("idBox not populated with true-pixel box: %+v", d.idBox)
	}
}

func boxedImage(w, h int, boxes ...image.Rectangle) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.White)
		}
	}
	for _, r := range boxes {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				img.Set(x, y, color.RGBA{R: 10, G: 10, B: 10, A: 255})
			}
		}
	}
	return img
}

func TestObserveRung2_SoM(t *testing.T) {
	// No a11y, but a screenshot with a detectable element → Rung 2 (marked screenshot,
	// refs are the numbered boxes).
	img := boxedImage(300, 200, image.Rect(40, 40, 120, 80))
	restore := withSeams(t, `[]`, img, "CanvasApp", nil)
	defer restore()
	d := newDriver()
	obs := d.observe(context.Background(), desktopwire.Act{Op: desktopwire.OpObserve}, "")
	if obs.Rung != desktopwire.RungSoM {
		t.Fatalf("rung = %d, want 2", obs.Rung)
	}
	if obs.ScreenshotB64 == "" {
		t.Fatal("rung 2 must carry a marked screenshot")
	}
	if len(obs.Refs) == 0 {
		t.Fatal("rung 2 must offer numbered refs")
	}
	if len(d.idBox) == 0 {
		t.Fatal("rung 2 must keep true-pixel boxes for clicking")
	}
}

func TestObserveRung3_Coordinate(t *testing.T) {
	// No a11y and a blank screenshot (nothing detectable) → Rung 3 (raw screenshot,
	// no refs, coordinate mode).
	restore := withSeams(t, `[]`, boxedImage(300, 200), "Game", nil)
	defer restore()
	d := newDriver()
	obs := d.observe(context.Background(), desktopwire.Act{Op: desktopwire.OpObserve}, "")
	if obs.Rung != desktopwire.RungCoordinate {
		t.Fatalf("rung = %d, want 3", obs.Rung)
	}
	if obs.ScreenshotB64 == "" || len(obs.Refs) != 0 {
		t.Fatalf("rung 3: want a screenshot and no refs, got refs=%d shot=%v", len(obs.Refs), obs.ScreenshotB64 != "")
	}
}

func TestPerformClickRef(t *testing.T) {
	var rec [][]string
	restore := withSeams(t, `[]`, nil, "", &rec)
	defer restore()
	d := newDriver()
	d.idBox[3] = desktopwire.Box{X: 100, Y: 100, W: 40, H: 20} // centre (120,110)
	if err := d.perform(context.Background(), desktopwire.Act{Op: desktopwire.OpClick, Ref: 3}); err != nil {
		t.Fatal(err)
	}
	if len(rec) != 1 || !reflect.DeepEqual(rec[0], xdotoolClick(120, 110)) {
		t.Fatalf("click recorded = %v, want click at (120,110)", rec)
	}
	// A ref not in the snapshot fails closed.
	if err := d.perform(context.Background(), desktopwire.Act{Op: desktopwire.OpClick, Ref: 9}); err == nil {
		t.Fatal("click on an unknown ref must fail closed")
	}
}

func TestPerformCoordinateRescale(t *testing.T) {
	var rec [][]string
	restore := withSeams(t, `[]`, nil, "", &rec)
	defer restore()
	d := newDriver()
	d.scaleX, d.scaleY = 2, 2 // resized→true ×2
	if err := d.perform(context.Background(), desktopwire.Act{Op: desktopwire.OpClick, Coordinate: []int{50, 25}}); err != nil {
		t.Fatal(err)
	}
	if len(rec) != 1 || !reflect.DeepEqual(rec[0], xdotoolClick(100, 50)) {
		t.Fatalf("coordinate click = %v, want true (100,50)", rec)
	}
}

// TestObserveDrivesLadder2to3FromRealMarkability proves B7-cu.4: a window with NO
// a11y refs and a BLANK screenshot (no CV boxes) drives the ladder's 2→3 transition
// with the REAL HasMarkableBoxes=false, so the per-window ladder cache records Rung 3,
// not a hardcoded-true Rung 2. We confirm by querying the ladder directly afterward.
func TestObserveDrivesLadder2to3FromRealMarkability(t *testing.T) {
	restore := withSeams(t, `[]`, boxedImage(300, 200), "BlankCanvas", nil)
	defer restore()
	d := newDriver()
	obs := d.observe(context.Background(), desktopwire.Act{Op: desktopwire.OpObserve}, "")
	if obs.Rung != desktopwire.RungCoordinate {
		t.Fatalf("blank canvas → rung %d, want 3", obs.Rung)
	}
	// The ladder itself must have been told markable=false for this window: a follow-up
	// Decide with the same (no-ref, no-mark) input still yields Rung 3 rather than the
	// Rung-2 a hardcoded HasMarkableBoxes:true would have produced.
	dec := d.ladder.Decide(desktop.RungInput{Window: "BlankCanvas", RefCount: 0, HasMarkableBoxes: false})
	if dec.Rung != desktopwire.RungCoordinate {
		t.Fatalf("ladder 2→3 not driven by real markability: got rung %d (%s)", dec.Rung, dec.Reason)
	}
}

// TestObserveStagnationDowngradesOnType proves B7-cu.5: a TYPE op (not a ref-click)
// that leaves the post-act signature equal to the pre-act one downgrades the window
// off Rung 1, just like a lying ref-click. A pre-fix driver only downgraded on
// ref-clicks, so a tree that lied for typing stayed latched on Rung 1.
func TestObserveStagnationDowngradesOnType(t *testing.T) {
	a11y := `[{"role":"entry","name":"Field","box":{"x":5,"y":5,"w":100,"h":24}}]`
	restore := withSeams(t, a11y, boxedImage(300, 200, image.Rect(40, 40, 120, 80)), "LyingTree", nil)
	defer restore()
	d := newDriver()

	// First observe establishes Rung 1 and records lastSig = sig("LyingTree", 1).
	first := d.observe(context.Background(), desktopwire.Act{Op: desktopwire.OpObserve}, "")
	if first.Rung != desktopwire.RungATSPI {
		t.Fatalf("first observe → rung %d, want 1", first.Rung)
	}
	preSig := d.lastSig

	// A TYPE op whose post-act signature equals preSig (same window, same ref count) is
	// a verified no-op → the tree lied → drop off Rung 1 to a marked screenshot (Rung 2).
	after := d.observe(context.Background(), desktopwire.Act{Op: desktopwire.OpType, Text: "x"}, preSig)
	if after.Rung == desktopwire.RungATSPI {
		t.Fatalf("a stagnant TYPE must downgrade off rung 1, got rung %d", after.Rung)
	}
}

func TestObserveNative_RawScreenshot(t *testing.T) {
	// In native mode (Path A) observe skips the a11y/SoM ladder and returns a raw
	// fixed-size screenshot with no refs — pixel-mode for Anthropic's native tool.
	restore := withSeams(t, `[{"role":"push button","name":"X","box":{"x":1,"y":1,"w":9,"h":9}}]`, boxedImage(1280, 800, image.Rect(40, 40, 120, 80)), "App", nil)
	defer restore()
	d := newDriver()
	d.native = true
	obs := d.observe(context.Background(), desktopwire.Act{Op: desktopwire.OpObserve}, "")
	if obs.Rung != desktopwire.RungCoordinate {
		t.Fatalf("native rung = %d, want 3 (coordinate)", obs.Rung)
	}
	if len(obs.Refs) != 0 {
		t.Fatal("native mode must not return refs (even though a11y had some) — pixel mode")
	}
	if obs.ScreenshotB64 == "" {
		t.Fatal("native mode must return a screenshot")
	}
	if d.scaleX != 1 || d.scaleY != 1 {
		t.Fatalf("native scale = (%v,%v), want 1:1", d.scaleX, d.scaleY)
	}
}
