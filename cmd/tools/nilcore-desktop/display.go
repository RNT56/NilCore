package main

import (
	"context"
	"os"
	"os/exec"
	"time"

	"nilcore/internal/desktopwire"
)

// observeNative is the Path-A observation: the raw screenshot at the FIXED display
// dimensions (desktopwire.NativeDisplayW×H — the Xvfb geometry), with NO a11y refs
// and NO SoM ladder — the native Anthropic model grounds from pixels itself, and the
// coordinates it returns map 1:1 (scale 1) onto the true display. The driver still
// owns actuation (xdotool); only perception is raw pixels here.
func (d *driver) observeNative(ctx context.Context) desktopwire.Observation {
	d.ver++
	d.scaleX, d.scaleY = 1, 1 // fixed display dims ⇒ 1:1 coordinates
	d.idBox = map[int]desktopwire.Box{}
	obs := desktopwire.Observation{Version: d.ver, Rung: desktopwire.RungCoordinate, FocusedWindow: collapse(activeWindow(ctx))}
	full, err := capture(ctx)
	if err != nil || full == nil {
		obs.Console = []string{"capture failed: " + errStr(err)}
		return obs
	}
	// The Xvfb geometry equals the budget, so capture is already NativeDisplayW×H; the
	// resize is a no-op that also guarantees the sent dims match the tool's declared
	// display_*_px (so Anthropic never 400s and coordinates stay 1:1).
	display, _, _ := resizeNearest(full, desktopwire.NativeDisplayW, desktopwire.NativeDisplayH)
	obs.ScreenshotB64 = pngB64(display)
	return obs
}

// xvfbGeometry is the fixed virtual-display geometry (matches desktopwire.NativeDisplayW×H
// and the native tool's declared dims).
const xvfbGeometry = "1280x800x24"

// ensureDisplay brings up the contained X11 desktop: Xvfb, a window manager, a panel,
// and the seed apps the eval scenarios target. It is the live, CI-only seam (a var so
// unit tests never reach it). Best-effort on the WM/apps; a missing Xvfb is fatal
// (the caller fails closed). Idempotent-ish: if DISPLAY already answers, it is a no-op.
var ensureDisplay = func(ctx context.Context) error {
	display := os.Getenv("DISPLAY")
	if display == "" {
		display = ":99"
		_ = os.Setenv("DISPLAY", display)
	}
	// If the display already answers (e.g. an entrypoint started it), do nothing.
	if exec.CommandContext(ctx, "xdotool", "getdisplaygeometry").Run() == nil {
		return nil
	}
	// Xvfb is required.
	xvfb := exec.Command("Xvfb", display, "-screen", "0", xvfbGeometry, "-nolisten", "tcp")
	if err := xvfb.Start(); err != nil {
		return err
	}
	// Wait briefly for the display to come up.
	for i := 0; i < 50; i++ {
		if exec.CommandContext(ctx, "xdotool", "getdisplaygeometry").Run() == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Best-effort WM + panel + seed apps (failures are non-fatal — the model can still
	// drive whatever rendered).
	_ = exec.Command("openbox").Start()
	time.Sleep(300 * time.Millisecond)
	_ = exec.Command("tint2").Start()
	for _, app := range []string{"gnome-calculator", "gnome-text-editor", "pcmanfm"} {
		if _, err := exec.LookPath(app); err == nil {
			_ = exec.Command(app).Start()
		}
	}
	time.Sleep(500 * time.Millisecond)
	return nil
}
