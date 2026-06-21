package main

import "image"

// This file is CU-MAC-T02: the points↔pixels↔resized coordinate model, owned in ONE
// place (the #1 mis-click bug). macOS is a two-stage map that Linux's single-stage
// rescale is not:
//
//	the model returns a coordinate in the RESIZED screenshot space,
//	  → resized→pixel   (× resizeScale, the downscale factor)
//	  → pixel→point     (÷ backingScale; Retina = 2.0)         [CGEvent/cliclick take POINTS]
//	  → + display origin (multi-monitor: a secondary display has a non-zero/negative origin)
//
// All pure arithmetic, unit-tested at 1×/2×/multi-monitor. No native code — the
// driver supplies the scale/origin (read once, re-read on display-config change).

// resizedToPoint maps a coordinate in the resized image space to a true macOS point.
// scaleX/scaleY are the per-axis resized→pixel factors (orig-pixel / resized);
// backingScale is pixel→point (Retina ≈ 2); origin is the display offset in points.
func resizedToPoint(x, y int, scaleX, scaleY, backingScale float64, originX, originY int) (int, int) {
	if backingScale <= 0 {
		backingScale = 1
	}
	px := float64(x) * scaleX
	py := float64(y) * scaleY
	return int(px/backingScale) + originX, int(py/backingScale) + originY
}

// pixelCenterToPoint maps a box (in capture PIXELS) to its centre in macOS points.
// Used for ref/mark clicks (Rung 2) where the box came from CV/AT-SPI on the pixel
// screenshot.
func pixelCenterToPoint(b image.Rectangle, backingScale float64, originX, originY int) (int, int) {
	if backingScale <= 0 {
		backingScale = 1
	}
	cx := float64(b.Min.X+b.Max.X) / 2
	cy := float64(b.Min.Y+b.Max.Y) / 2
	return int(cx/backingScale) + originX, int(cy/backingScale) + originY
}
