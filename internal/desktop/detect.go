// Package desktop holds the pure-Go perception+decision helpers the nilcore-desktop
// driver uses for the Set-of-Marks ladder (Phase CU): classical-CV box detection
// (detect.go, Rung-2 fallback box source) and the per-step rung decision
// (ladder.go). Everything here is stdlib-only arithmetic over image.Image — ZERO
// module, no ML, CGO-free (I6) — so it is hermetically testable and the driver stays
// a thin orchestrator over it.
package desktop

import (
	"image"
	"sort"
)

// DetectOptions bounds the classical-CV box proposals so a noisy screen cannot
// flood the model with marks.
type DetectOptions struct {
	EdgeThreshold int     // gradient magnitude (0..255) above which a pixel is an edge
	DilateIters   int     // how many 3×3 dilations to connect nearby edges
	MinW, MinH    int     // smallest acceptable box (drop sub-glyph noise)
	MaxAreaFrac   float64 // largest box as a fraction of the image (drop full-window blobs)
	MaxBoxes      int     // hard cap on returned boxes
}

// DefaultDetectOptions are sane starting points; the eval flywheel (CU-T10) tunes them.
func DefaultDetectOptions() DetectOptions {
	return DetectOptions{EdgeThreshold: 48, DilateIters: 2, MinW: 12, MinH: 10, MaxAreaFrac: 0.5, MaxBoxes: 60}
}

// Detect proposes candidate interactive boxes from a screenshot when AT-SPI gives
// nothing. It is a degraded fallback (no semantics, can over/under-segment) — the
// driver prefers AT-SPI boxes and treats these as best-effort. Boxes are returned
// largest-first then top-left, then capped.
func Detect(img image.Image, opt DetectOptions) []image.Rectangle {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	gray := toGray(img)
	edges := edgeMap(gray, w, h, opt.EdgeThreshold)
	for i := 0; i < opt.DilateIters; i++ {
		edges = dilate(edges, w, h)
	}
	boxes := components(edges, w, h)

	maxArea := int(opt.MaxAreaFrac * float64(w*h))
	out := make([]image.Rectangle, 0, len(boxes))
	for _, r := range boxes {
		if r.Dx() < opt.MinW || r.Dy() < opt.MinH {
			continue
		}
		if maxArea > 0 && r.Dx()*r.Dy() > maxArea {
			continue
		}
		out = append(out, r.Add(b.Min)) // shift back into the image's coordinate space
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := out[i].Dx()*out[i].Dy(), out[j].Dx()*out[j].Dy()
		if ai != aj {
			return ai > aj // largest first
		}
		if out[i].Min.Y != out[j].Min.Y {
			return out[i].Min.Y < out[j].Min.Y
		}
		return out[i].Min.X < out[j].Min.X
	})
	if opt.MaxBoxes > 0 && len(out) > opt.MaxBoxes {
		out = out[:opt.MaxBoxes]
	}
	return out
}

// toGray converts to an 8-bit luma grid indexed [y*w+x].
func toGray(img image.Image) []uint8 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	g := make([]uint8, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, gg, bb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			// Rec.601 luma; RGBA() returns 16-bit, shift to 8.
			lum := (299*(r>>8) + 587*(gg>>8) + 114*(bb>>8)) / 1000
			g[y*w+x] = uint8(lum)
		}
	}
	return g
}

// edgeMap marks pixels whose gradient magnitude (|dx|+|dy|) exceeds the threshold.
func edgeMap(gray []uint8, w, h, thr int) []bool {
	e := make([]bool, w*h)
	at := func(x, y int) int { return int(gray[y*w+x]) }
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			dx := abs(at(x+1, y) - at(x-1, y))
			dy := abs(at(x, y+1) - at(x, y-1))
			if dx+dy >= thr {
				e[y*w+x] = true
			}
		}
	}
	return e
}

// dilate grows the binary edge map by a 3×3 structuring element (connects nearby edges).
func dilate(in []bool, w, h int) []bool {
	out := make([]bool, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if in[y*w+x] {
				out[y*w+x] = true
				continue
			}
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny := x+dx, y+dy
					if nx >= 0 && nx < w && ny >= 0 && ny < h && in[ny*w+nx] {
						out[y*w+x] = true
					}
				}
			}
		}
	}
	return out
}

// components labels 8-connected true regions (BFS) and returns each region's
// bounding rectangle, in image-local coordinates.
func components(bin []bool, w, h int) []image.Rectangle {
	seen := make([]bool, w*h)
	var rects []image.Rectangle
	queue := make([]int, 0, 256)
	for start := 0; start < w*h; start++ {
		if !bin[start] || seen[start] {
			continue
		}
		// BFS flood-fill this component, tracking its bounds.
		minX, minY := w, h
		maxX, maxY := 0, 0
		queue = queue[:0]
		queue = append(queue, start)
		seen[start] = true
		for len(queue) > 0 {
			p := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			px, py := p%w, p/w
			if px < minX {
				minX = px
			}
			if py < minY {
				minY = py
			}
			if px > maxX {
				maxX = px
			}
			if py > maxY {
				maxY = py
			}
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny := px+dx, py+dy
					if nx < 0 || nx >= w || ny < 0 || ny >= h {
						continue
					}
					q := ny*w + nx
					if bin[q] && !seen[q] {
						seen[q] = true
						queue = append(queue, q)
					}
				}
			}
		}
		rects = append(rects, image.Rect(minX, minY, maxX+1, maxY+1))
	}
	return rects
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
