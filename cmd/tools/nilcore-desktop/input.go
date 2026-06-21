package main

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"nilcore/internal/desktopwire"
)

// This file holds the input/capture seams. The xdotool ARG BUILDERS are pure and
// unit-tested; the actual exec (runXdotool) and screen capture (capture) are live
// seams (CI-only). resizeNearest is pure stdlib (no module, I6).

// xdotoolClick builds args for a left click at true-screen (x,y).
func xdotoolClick(x, y int) []string {
	return []string{"mousemove", "--sync", strconv.Itoa(x), strconv.Itoa(y), "click", "1"}
}

// xdotoolType builds args to type literal text at the current focus. `--` ends
// option parsing so text beginning with `-` is data, not a flag.
func xdotoolType(text string) []string {
	return []string{"type", "--clearmodifiers", "--", text}
}

// xdotoolKey builds args for a key chord like "ctrl+s" or "Return". xdotool's key
// syntax IS the chord; we pass it as a single token (the host validated it as data).
func xdotoolKey(chord string) []string {
	return []string{"key", "--clearmodifiers", chord}
}

// xdotoolScroll builds args to scroll: xdotool maps wheel to buttons 4(up)/5(down)/
// 6(left)/7(right); amount becomes the repeat count (>=1).
func xdotoolScroll(dir string, amount int) []string {
	if amount < 1 {
		amount = 3
	}
	button := "5" // down (default)
	switch dir {
	case "up":
		button = "4"
	case "left":
		button = "6"
	case "right":
		button = "7"
	}
	return []string{"click", "--repeat", strconv.Itoa(amount), button}
}

// runXdotool is the live seam (CI-only). A var so tests substitute a recorder.
var runXdotool = func(ctx context.Context, args []string) error {
	return exec.CommandContext(ctx, "xdotool", args...).Run()
}

// capture is the live screen-capture seam (CI-only) — returns the full desktop as an
// image. A var so tests substitute a fake image. The production impl shells to scrot.
var capture = func(ctx context.Context) (image.Image, error) {
	return captureScrot(ctx)
}

// rescaleCoord maps a coordinate in the RESIZED image space back to true display
// pixels (the #1 mis-click bug, owned in ONE place). scale = trueDim/resizedDim.
func rescaleCoord(x, y int, scaleX, scaleY float64) (int, int) {
	return int(float64(x) * scaleX), int(float64(y) * scaleY)
}

// resizeNearest downscales src to fit within (maxW,maxH) preserving aspect, using
// pure-stdlib nearest-neighbour (no module). It returns the resized image and the
// scale factors (true/resized) so a returned coordinate can be mapped back.
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

// scaleBoxToResized maps a true-pixel box into the resized image space for the SoM
// overlay (the inverse of rescaleCoord).
func scaleBoxToResized(b desktopwire.Box, scaleX, scaleY float64) image.Rectangle {
	x0 := int(float64(b.X) / scaleX)
	y0 := int(float64(b.Y) / scaleY)
	x1 := int(float64(b.X+b.W) / scaleX)
	y1 := int(float64(b.Y+b.H) / scaleY)
	return image.Rect(x0, y0, x1, y1)
}

// captureScrot is the production capture: shell to scrot writing a PNG to a temp
// file, then decode it (CI-only). Kept behind the `capture` var so unit tests never
// reach it.
func captureScrot(ctx context.Context) (image.Image, error) {
	dir, err := os.MkdirTemp("", "nilcore-desktop-shot-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "shot.png")
	if err := exec.CommandContext(ctx, "scrot", "-o", p).Run(); err != nil {
		return nil, fmt.Errorf("scrot capture: %w", err)
	}
	f, err := os.Open(p) //nolint:gosec // temp path we created
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}
