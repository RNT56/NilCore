package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// This file is CU-MAC-T03: capture via the OS-baked `screencapture` (Apple-maintained,
// no Screen-Recording prompt on the CLI historically, survives the macOS-15
// CGWindowList obsoletion), plus Retina backing-scale detection and the pure-stdlib
// resize/encode. The capture is in PIXELS; the backing scale lets coords.go map a
// model coordinate back to macOS POINTS.

// capture is the live screen-capture seam (CI/host-only). A var so tests substitute.
var capture = func(ctx context.Context) (image.Image, error) {
	return captureScreencapture(ctx)
}

// captureScreencapture shells to `screencapture -x -t png <tmp>` (silent, PNG) and
// decodes the result. A non-zero exit (no Screen-Recording grant) surfaces as the
// error — the driver then fails closed, never fabricating a frame.
func captureScreencapture(ctx context.Context) (image.Image, error) {
	dir, err := os.MkdirTemp("", "nilcore-mac-shot-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "shot.png")
	if err := exec.CommandContext(ctx, "screencapture", "-x", "-t", "png", p).Run(); err != nil {
		return nil, err
	}
	f, err := os.Open(p) //nolint:gosec // temp path we created
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

// backingScale resolves the display's Retina backing-scale factor (pixel/point):
// NILCORE_MAC_SCALE env first, then a best-effort osascript probe (desktop point
// width vs the captured pixel width), then 2.0 (the Apple-Silicon Retina default).
// It is a var so the live osascript path can be faked in tests.
var backingScale = func(ctx context.Context, pixelW int) float64 {
	if v := strings.TrimSpace(os.Getenv("NILCORE_MAC_SCALE")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 1 && f <= 4 {
			return f
		}
	}
	if pointW := osascriptDesktopWidth(ctx); pointW > 0 && pixelW > 0 {
		s := float64(pixelW) / float64(pointW)
		if s >= 1 && s <= 4 {
			return s
		}
	}
	return 2.0
}

// osascriptDesktopWidth reads the desktop point width via AppleScript (no CGO). It
// may require Automation permission; on failure it returns 0 and the caller falls
// back. The output looks like "0, 0, 1512, 982" → the 3rd value is the width.
func osascriptDesktopWidth(ctx context.Context) int {
	out, err := exec.CommandContext(ctx, "osascript", "-e", "tell application \"Finder\" to get bounds of window of desktop").Output()
	if err != nil {
		return 0
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(fields) < 3 {
		return 0
	}
	w, err := strconv.Atoi(strings.TrimSpace(fields[2]))
	if err != nil {
		return 0
	}
	return w
}

// resizeNearest downscales src to fit (maxW,maxH) preserving aspect, pure-stdlib
// nearest-neighbour. Returns the resized image and the scale factors (orig-pixel /
// resized) so a returned coordinate maps back to capture pixels.
func resizeNearest(src image.Image, maxW, maxH int) (*image.RGBA, float64, float64) {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1)), 1, 1
	}
	dw, dh := sw, sh
	if sw > maxW || sh > maxH {
		rx := float64(maxW) / float64(sw)
		ry := float64(maxH) / float64(sh)
		r := rx
		if ry < r {
			r = ry
		}
		dw = int(float64(sw) * r)
		dh = int(float64(sh) * r)
		if dw < 1 {
			dw = 1
		}
		if dh < 1 {
			dh = 1
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	scaleX := float64(sw) / float64(dw)
	scaleY := float64(sh) / float64(dh)
	for y := 0; y < dh; y++ {
		sy := b.Min.Y + int(float64(y)*scaleY)
		for x := 0; x < dw; x++ {
			sx := b.Min.X + int(float64(x)*scaleX)
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst, scaleX, scaleY
}

// pngB64 encodes an image to base64 PNG.
func pngB64(img image.Image) string {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
