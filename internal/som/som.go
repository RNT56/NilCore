// Package som is the stdlib-only Set-of-Marks overlay for the desktop computer-use
// ladder (Phase CU, CU-T06, Rung 2). It draws numbered boxes onto a screenshot so a
// multimodal model picks a NUMBER ("[5]") instead of guessing raw coordinates —
// turning coordinate-regression into multiple-choice (Set-of-Mark, arXiv:2310.11441).
//
// It is a pure leaf: image/draw + a tiny embedded 5×7 digit bitmap, so it adds ZERO
// Go module (I6) and no font dependency. The box sources (AT-SPI extents, classical
// CV) are the caller's (the driver); this package only marks + returns the id→box
// map the driver logs (I5) and uses to map a picked "[N]" back to a click point.
package som

import (
	"image"
	"image/color"
	"image/draw"
)

// Mark is one numbered box to overlay. Box is in the image's pixel space.
type Mark struct {
	ID    int
	Box   image.Rectangle
	Role  string
	Label string
}

// markColor is the box/badge colour — a high-contrast magenta that rarely collides
// with UI chrome.
var (
	markColor = color.RGBA{R: 255, G: 0, B: 255, A: 255}
	badgeBG   = color.RGBA{R: 0, G: 0, B: 0, A: 255}
	badgeFG   = color.RGBA{R: 255, G: 255, B: 0, A: 255}
)

// digitFont is a 5×7 bitmap for '0'–'9'; each row's low 5 bits are pixels (bit4 = leftmost).
var digitFont = [10][7]byte{
	{0b01110, 0b10001, 0b10011, 0b10101, 0b11001, 0b10001, 0b01110}, // 0
	{0b00100, 0b01100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110}, // 1
	{0b01110, 0b10001, 0b00001, 0b00010, 0b00100, 0b01000, 0b11111}, // 2
	{0b11111, 0b00010, 0b00100, 0b00010, 0b00001, 0b10001, 0b01110}, // 3
	{0b00010, 0b00110, 0b01010, 0b10010, 0b11111, 0b00010, 0b00010}, // 4
	{0b11111, 0b10000, 0b11110, 0b00001, 0b00001, 0b10001, 0b01110}, // 5
	{0b00110, 0b01000, 0b10000, 0b11110, 0b10001, 0b10001, 0b01110}, // 6
	{0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b01000, 0b01000}, // 7
	{0b01110, 0b10001, 0b10001, 0b01110, 0b10001, 0b10001, 0b01110}, // 8
	{0b01110, 0b10001, 0b10001, 0b01111, 0b00001, 0b00010, 0b01100}, // 9
}

// Overlay copies src, draws each mark's box border + a numbered badge, and returns
// the marked image plus the id→box map. The map is what the driver logs and uses to
// turn a model-picked "[N]" into a click point (the box centre). Boxes are clamped
// to the image bounds; a mark whose box is empty/out-of-frame is skipped.
func Overlay(src image.Image, marks []Mark) (*image.RGBA, map[int]image.Rectangle) {
	b := src.Bounds()
	dst := image.NewRGBA(b)
	draw.Draw(dst, b, src, b.Min, draw.Src)

	idbox := make(map[int]image.Rectangle, len(marks))
	for _, m := range marks {
		r := m.Box.Intersect(b)
		if r.Empty() {
			continue
		}
		drawBorder(dst, r, markColor, 2)
		drawBadge(dst, r.Min.X, r.Min.Y, m.ID, b)
		idbox[m.ID] = r
	}
	return dst, idbox
}

// drawBorder draws a w-pixel border of c around r (clamped to the image).
func drawBorder(dst *image.RGBA, r image.Rectangle, c color.RGBA, w int) {
	for i := 0; i < w; i++ {
		top := image.Rect(r.Min.X, r.Min.Y+i, r.Max.X, r.Min.Y+i+1)
		bot := image.Rect(r.Min.X, r.Max.Y-i-1, r.Max.X, r.Max.Y-i)
		left := image.Rect(r.Min.X+i, r.Min.Y, r.Min.X+i+1, r.Max.Y)
		right := image.Rect(r.Max.X-i-1, r.Min.Y, r.Max.X-i, r.Max.Y)
		for _, e := range []image.Rectangle{top, bot, left, right} {
			draw.Draw(dst, e.Intersect(dst.Bounds()), &image.Uniform{C: c}, image.Point{}, draw.Src)
		}
	}
}

// drawBadge draws the id number as filled-background digits at (x,y), clamped so it
// stays inside the image.
func drawBadge(dst *image.RGBA, x, y, id int, bounds image.Rectangle) {
	digits := digitsOf(id)
	const dw, dh, pad = 5, 7, 1
	bw := pad*2 + len(digits)*(dw+1)
	bh := pad*2 + dh
	// Clamp the badge fully inside the image.
	if x+bw > bounds.Max.X {
		x = bounds.Max.X - bw
	}
	if y+bh > bounds.Max.Y {
		y = bounds.Max.Y - bh
	}
	if x < bounds.Min.X {
		x = bounds.Min.X
	}
	if y < bounds.Min.Y {
		y = bounds.Min.Y
	}
	draw.Draw(dst, image.Rect(x, y, x+bw, y+bh), &image.Uniform{C: badgeBG}, image.Point{}, draw.Src)
	cx := x + pad
	for _, d := range digits {
		drawDigit(dst, cx, y+pad, d)
		cx += dw + 1
	}
}

// drawDigit sets the foreground pixels of one digit at (x,y).
func drawDigit(dst *image.RGBA, x, y, d int) {
	if d < 0 || d > 9 {
		return
	}
	rows := digitFont[d]
	for ry := 0; ry < 7; ry++ {
		bits := rows[ry]
		for rx := 0; rx < 5; rx++ {
			if bits&(1<<(4-rx)) != 0 {
				dst.Set(x+rx, y+ry, badgeFG)
			}
		}
	}
}

// digitsOf returns id's decimal digits (id<0 → "0").
func digitsOf(id int) []int {
	if id <= 0 {
		return []int{0}
	}
	var out []int
	for id > 0 {
		out = append([]int{id % 10}, out...)
		id /= 10
	}
	return out
}
