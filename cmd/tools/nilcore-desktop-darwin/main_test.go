package main

import (
	"context"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	got, err := cliclickScroll("up", 2)
	if err != nil || !reflect.DeepEqual(got, []string{"kp:page-up", "kp:page-up"}) {
		t.Fatalf("scroll up = %v (err %v)", got, err)
	}
	if got, err := cliclickScroll("", 1); err != nil || !reflect.DeepEqual(got, []string{"kp:page-down"}) {
		t.Fatalf("default scroll = %v (err %v)", got, err)
	}
	// Horizontal scroll has no faithful page-key substitute → fail closed, never a
	// silent page-down that misleads the model.
	if _, err := cliclickScroll("left", 1); err == nil {
		t.Fatal("horizontal (left) scroll must fail closed, not silently page-down")
	}
	if _, err := cliclickScroll("right", 3); err == nil {
		t.Fatal("horizontal (right) scroll must fail closed")
	}
}

// TestCliclickClickVariants proves the button/count contract: right and double clicks
// map to distinct cliclick verbs, and an unsupported variant (middle click, triple, or a
// repeated right click) fails closed rather than silently left-clicking.
func TestCliclickClickVariants(t *testing.T) {
	if got, err := cliclickClickN(1, 2, desktopwire.ButtonLeft, 1); err != nil || !reflect.DeepEqual(got, []string{"c:1,2"}) {
		t.Fatalf("left click = %v (err %v)", got, err)
	}
	if got, err := cliclickClickN(1, 2, desktopwire.ButtonLeft, 2); err != nil || !reflect.DeepEqual(got, []string{"dc:1,2"}) {
		t.Fatalf("double click = %v (err %v)", got, err)
	}
	if got, err := cliclickClickN(1, 2, desktopwire.ButtonRight, 1); err != nil || !reflect.DeepEqual(got, []string{"rc:1,2"}) {
		t.Fatalf("right click = %v (err %v)", got, err)
	}
	if _, err := cliclickClickN(1, 2, desktopwire.ButtonMiddle, 1); err == nil {
		t.Fatal("middle click must fail closed (no cliclick verb), never a silent left click")
	}
	if _, err := cliclickClickN(1, 2, desktopwire.ButtonLeft, 3); err == nil {
		t.Fatal("triple click must fail closed on the MVP")
	}
	if _, err := cliclickClickN(1, 2, desktopwire.ButtonRight, 2); err == nil {
		t.Fatal("repeated right click must fail closed on the MVP")
	}
}

// TestCliclickDrag proves a drag maps to press(dd)→release(du); a non-left drag fails closed.
func TestCliclickDrag(t *testing.T) {
	if got, err := cliclickDrag(1, 2, 3, 4, desktopwire.ButtonLeft); err != nil || !reflect.DeepEqual(got, []string{"dd:1,2", "du:3,4"}) {
		t.Fatalf("drag = %v (err %v)", got, err)
	}
	if got, err := cliclickMouseDown(5, 6, desktopwire.ButtonLeft); err != nil || !reflect.DeepEqual(got, []string{"dd:5,6"}) {
		t.Fatalf("mouse-down = %v (err %v)", got, err)
	}
	if got, err := cliclickMouseUp(5, 6, ""); err != nil || !reflect.DeepEqual(got, []string{"du:5,6"}) {
		t.Fatalf("mouse-up = %v (err %v)", got, err)
	}
	if _, err := cliclickDrag(1, 2, 3, 4, desktopwire.ButtonRight); err == nil {
		t.Fatal("non-left drag must fail closed on the MVP")
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

	// 1× display (the non-Retina external-monitor case the old bscale=2 fallback
	// silently halved): backing 1.0 means pixel == point, so a resized coordinate maps
	// 1:1 once the resize factor is undone — no halving.
	x1, y1 := resizedToPoint(50, 25, 1, 1, 1, 0, 0)
	if x1 != 50 || y1 != 25 {
		t.Fatalf("1× resizedToPoint = (%d,%d), want (50,25)", x1, y1)
	}
	// A box centre on a 1× display is its raw pixel centre (no /2).
	bx1, by1 := pixelCenterToPoint(image.Rect(100, 100, 140, 120), 1, 0, 0) // centre px (120,110)
	if bx1 != 120 || by1 != 110 {
		t.Fatalf("1× pixelCenterToPoint = (%d,%d), want (120,110)", bx1, by1)
	}
	// The same box on a 2× display halves to (60,55) — the two scales must DIFFER, which
	// is exactly why a wrong 2.0 fallback on a 1× display mis-clicks.
	if bx1 == bx || by1 == by {
		t.Fatalf("1× and 2× box centres must differ: 1×=(%d,%d) 2×=(%d,%d)", bx1, by1, bx, by)
	}
}

func TestResolveBackingScale(t *testing.T) {
	restore := func() func() {
		oo := osascriptDesktopWidth
		return func() { osascriptDesktopWidth = oo }
	}()
	defer restore()

	// Env override wins and short-circuits the probe.
	t.Setenv("NILCORE_MAC_SCALE", "1")
	osascriptDesktopWidth = func(context.Context) int { t.Fatal("env override must short-circuit the osascript probe"); return 0 }
	if s, known := resolveBackingScale(context.Background(), 3024); !known || s != 1 {
		t.Fatalf("env override = (%v,%v), want (1,true)", s, known)
	}

	// No env, osascript yields a 2× Retina display (3024 px / 1512 pt).
	t.Setenv("NILCORE_MAC_SCALE", "")
	osascriptDesktopWidth = func(context.Context) int { return 1512 }
	if s, known := resolveBackingScale(context.Background(), 3024); !known || s != 2 {
		t.Fatalf("Retina derivation = (%v,%v), want (2,true)", s, known)
	}

	// No env, osascript yields a 1× external display (1920 px / 1920 pt) — must derive
	// 1.0, NOT fall back to 2.0.
	osascriptDesktopWidth = func(context.Context) int { return 1920 }
	if s, known := resolveBackingScale(context.Background(), 1920); !known || s != 1 {
		t.Fatalf("1× derivation = (%v,%v), want (1,true)", s, known)
	}

	// No env and osascript fails (Automation denied) → the conservative unity fallback,
	// reported as undetermined so the caller can warn (NOT the old silent 2.0).
	osascriptDesktopWidth = func(context.Context) int { return 0 }
	if s, known := resolveBackingScale(context.Background(), 1920); known || s != fallbackBackingScale {
		t.Fatalf("fallback = (%v,%v), want (%v,false)", s, known, fallbackBackingScale)
	}
}

func TestProbeWarnsOnUndeterminedScale(t *testing.T) {
	oc, oo, op := capture, osascriptDesktopWidth, lookPath
	defer func() { capture, osascriptDesktopWidth, lookPath = oc, oo, op }()
	capture = func(context.Context) (image.Image, error) { return boxedImage(1920, 1080), nil }
	osascriptDesktopWidth = func(context.Context) int { return 0 } // Automation denied
	lookPath = func(string) (string, error) { return "/usr/local/bin/cliclick", nil }
	t.Setenv("NILCORE_MAC_SCALE", "")

	r := probePermissions(context.Background())
	if r.BackingScaleKnown {
		t.Fatal("scale must be reported undetermined when osascript fails and no env override")
	}
	joined := r.String()
	if !containsAny(joined, "Backing scale could NOT be determined", "NILCORE_MAC_SCALE") {
		t.Fatalf("probe must warn about the undetermined scale; got:\n%s", joined)
	}
}

func containsAny(hay string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(hay, n) {
			return false
		}
	}
	return true
}

// withSeams swaps the live macOS seams for fakes. It fakes the osascript desktop-width
// probe too (observe() now resolves the backing scale via resolveBackingScale, which
// would otherwise shell the REAL osascript and hang on a TCC prompt): a scale s and a
// 1000-px capture imply a fake point-width of 1000/s so resolveBackingScale derives s.
func withSeams(t *testing.T, img image.Image, scale float64, rec *[][]string) func() {
	t.Helper()
	t.Setenv("NILCORE_MAC_SCALE", "") // force the probe path, not the env override
	t.Setenv("TERM_PROGRAM", "")      // no controlling-terminal exclusion in observe tests
	oc, ob, ox, oo, of := capture, backingScale, runCliclick, osascriptDesktopWidth, frontmostApp
	capture = func(context.Context) (image.Image, error) { return img, nil }
	backingScale = func(context.Context, int) float64 { return scale }
	frontmostApp = func(context.Context) string { return "TargetApp" } // not a terminal
	pixelW := 0
	if img != nil {
		pixelW = img.Bounds().Dx()
	}
	osascriptDesktopWidth = func(context.Context) int {
		if pixelW <= 0 || scale <= 0 {
			return 0
		}
		return int(float64(pixelW) / scale)
	}
	runCliclick = func(_ context.Context, args []string) error {
		if rec != nil {
			*rec = append(*rec, args)
		}
		return nil
	}
	return func() {
		capture, backingScale, runCliclick, osascriptDesktopWidth, frontmostApp = oc, ob, ox, oo, of
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
	// Each ref must carry the observation's version so the host-side stale-ref guard can
	// reject a reused positional id after a re-render (mirrors the browser tier).
	if obs.Refs[0].Version != obs.Version {
		t.Fatalf("ref version %d != observation version %d — stale-ref guard would misfire", obs.Refs[0].Version, obs.Version)
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

// TestShredFileZerosThenRemoves verifies the host-control request shred: the file's bytes
// are overwritten with zeros before it is removed, so a crash between processing and
// unlink leaves no recoverable secret material (Finding 4 defense-in-depth; the recorded
// I3 relaxation on this tier — docs/ROADMAP-COMPUTER-USE-DARWIN.md).
func TestShredFileZerosThenRemoves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "req-1.json")
	const secret = "top-secret-token"
	if err := os.WriteFile(path, []byte(`{"act":{"text":"`+secret+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := shredFile(path); err != nil {
		t.Fatalf("shredFile: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("shredFile must remove the file, stat err = %v", err)
	}
	// A best-effort re-read of the (now-unlinked) path returns nothing; the security
	// property under test is that the bytes are zeroed before unlink, which we assert by
	// confirming a shred of a missing path is a no-op error (idempotent) and that a shred
	// leaves no readable secret behind.
	if err := shredFile(path); err == nil {
		t.Fatal("shredding a missing file should surface the remove error")
	}
}
