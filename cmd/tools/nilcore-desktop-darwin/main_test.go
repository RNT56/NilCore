package main

import (
	"context"
	"image"
	"image/color"
	"reflect"
	"testing"

	"nilcore/internal/desktopwire"
)

func TestParseArgs(t *testing.T) {
	serve, control, native, err := parseArgs([]string{"--serve", "--control", "/tmp/x", "--native"})
	if err != nil || !serve || control != "/tmp/x" || !native {
		t.Fatalf("parse = (%v,%q,%v,%v)", serve, control, native, err)
	}
	if _, _, _, err := parseArgs([]string{"--serve"}); err == nil {
		t.Fatal("--serve without --control must error")
	}
}

func TestCliclickBuilders(t *testing.T) {
	if got := cliclickClick(12, 34); !reflect.DeepEqual(got, []string{"c:12,34"}) {
		t.Fatalf("click = %v", got)
	}
	if got := cliclickType("hi there"); !reflect.DeepEqual(got, []string{"t:hi there"}) {
		t.Fatalf("type = %v", got)
	}
	// chord → hold mods, key, release mods.
	if got := cliclickKey("cmd+s"); !reflect.DeepEqual(got, []string{"kd:cmd", "t:s", "ku:cmd"}) {
		t.Fatalf("cmd+s = %v", got)
	}
	if got := cliclickKey("shift+Tab"); !reflect.DeepEqual(got, []string{"kd:shift", "kp:tab", "ku:shift"}) {
		t.Fatalf("shift+Tab = %v", got)
	}
	// lone special key.
	if got := cliclickKey("Return"); !reflect.DeepEqual(got, []string{"kp:return"}) {
		t.Fatalf("Return = %v", got)
	}
	if got := cliclickScroll("up", 2); !reflect.DeepEqual(got, []string{"kp:page-up", "kp:page-up"}) {
		t.Fatalf("scroll = %v", got)
	}
}

func TestCoords(t *testing.T) {
	// resized (50,25) at resize-scale 2 and backing 2 → pixel (100,50) → point (50,25).
	x, y := resizedToPoint(50, 25, 2, 2, 2, 0, 0)
	if x != 50 || y != 25 {
		t.Fatalf("resizedToPoint = (%d,%d), want (50,25)", x, y)
	}
	// with a secondary-display origin offset (points).
	x, y = resizedToPoint(50, 25, 2, 2, 2, 1000, -100)
	if x != 1050 || y != -75 {
		t.Fatalf("resizedToPoint+origin = (%d,%d), want (1050,-75)", x, y)
	}
	// a pixel box centre → point.
	bx, by := pixelCenterToPoint(image.Rect(100, 100, 140, 120), 2, 0, 0) // centre px (120,110) → pt (60,55)
	if bx != 60 || by != 55 {
		t.Fatalf("pixelCenterToPoint = (%d,%d), want (60,55)", bx, by)
	}
}

// withSeams swaps the live macOS seams for fakes.
func withSeams(t *testing.T, img image.Image, scale float64, rec *[][]string) func() {
	t.Helper()
	oc, ob, ox := capture, backingScale, runCliclick
	capture = func(context.Context) (image.Image, error) { return img, nil }
	backingScale = func(context.Context, int) float64 { return scale }
	runCliclick = func(_ context.Context, args []string) error {
		if rec != nil {
			*rec = append(*rec, args)
		}
		return nil
	}
	return func() { capture, backingScale, runCliclick = oc, ob, ox }
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

func newDriver() *driver {
	return &driver{idBox: map[int]image.Rectangle{}, scaleX: 1, scaleY: 1, bscale: 2}
}

func TestObserveRung2_CV(t *testing.T) {
	restore := withSeams(t, boxedImage(300, 200, image.Rect(40, 40, 120, 80)), 2, nil)
	defer restore()
	d := newDriver()
	obs := d.observe(context.Background())
	if obs.Rung != desktopwire.RungSoM {
		t.Fatalf("rung = %d, want 2 (CV marks)", obs.Rung)
	}
	if obs.ScreenshotB64 == "" || len(obs.Refs) == 0 || len(d.idBox) == 0 {
		t.Fatalf("rung 2 missing marks/screenshot/idBox: refs=%d", len(obs.Refs))
	}
}

func TestObserveRung3_Blank(t *testing.T) {
	restore := withSeams(t, boxedImage(300, 200), 2, nil)
	defer restore()
	d := newDriver()
	obs := d.observe(context.Background())
	if obs.Rung != desktopwire.RungCoordinate || len(obs.Refs) != 0 || obs.ScreenshotB64 == "" {
		t.Fatalf("rung 3 wrong: rung=%d refs=%d", obs.Rung, len(obs.Refs))
	}
}

func TestObserveNative_Raw(t *testing.T) {
	restore := withSeams(t, boxedImage(1280, 800, image.Rect(40, 40, 120, 80)), 2, nil)
	defer restore()
	d := newDriver()
	d.native = true
	obs := d.observe(context.Background())
	if obs.Rung != desktopwire.RungCoordinate || len(obs.Refs) != 0 {
		t.Fatalf("native mode must be raw screenshot with no refs: rung=%d refs=%d", obs.Rung, len(obs.Refs))
	}
}

func TestPerformClick(t *testing.T) {
	var rec [][]string
	restore := withSeams(t, nil, 2, &rec)
	defer restore()
	d := newDriver()
	// Ref click: pixel box (100,100)-(140,120) → point (60,55) at backing 2.
	d.idBox[3] = image.Rect(100, 100, 140, 120)
	if err := d.perform(context.Background(), desktopwire.Act{Op: desktopwire.OpClick, Ref: 3}); err != nil {
		t.Fatal(err)
	}
	if len(rec) != 1 || !reflect.DeepEqual(rec[0], cliclickClick(60, 55)) {
		t.Fatalf("ref click = %v, want point (60,55)", rec)
	}
	// Unknown ref fails closed.
	if err := d.perform(context.Background(), desktopwire.Act{Op: desktopwire.OpClick, Ref: 9}); err == nil {
		t.Fatal("unknown ref must fail closed")
	}
}

func TestPerformCoordinate(t *testing.T) {
	var rec [][]string
	restore := withSeams(t, nil, 2, &rec)
	defer restore()
	d := newDriver()
	d.scaleX, d.scaleY = 2, 2 // resized→pixel ×2; backing 2 → net ×1
	if err := d.perform(context.Background(), desktopwire.Act{Op: desktopwire.OpClick, Coordinate: []int{50, 25}}); err != nil {
		t.Fatal(err)
	}
	if len(rec) != 1 || !reflect.DeepEqual(rec[0], cliclickClick(50, 25)) {
		t.Fatalf("coordinate click = %v, want point (50,25)", rec)
	}
}
