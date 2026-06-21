package som

import (
	"image"
	"image/color"
	"testing"
)

func whiteImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.White)
		}
	}
	return img
}

func TestOverlayMarksAndMap(t *testing.T) {
	src := whiteImage(120, 100)
	marks := []Mark{
		{ID: 1, Box: image.Rect(10, 10, 60, 40)},
		{ID: 27, Box: image.Rect(70, 50, 110, 90)},
	}
	dst, idbox := Overlay(src, marks)

	// id→box map is exact.
	if idbox[1] != image.Rect(10, 10, 60, 40) {
		t.Fatalf("idbox[1] = %v", idbox[1])
	}
	if idbox[27] != image.Rect(70, 50, 110, 90) {
		t.Fatalf("idbox[27] = %v", idbox[27])
	}
	// A top-border pixel AWAY from the corner badge is the mark colour (was white).
	if r, g, b, _ := dst.At(35, 10).RGBA(); !(r > 0xf000 && g < 0x1000 && b > 0xf000) {
		t.Fatalf("border pixel not magenta: %v", dst.At(35, 10))
	}
	// The source is untouched (Overlay copies): the border was drawn on dst, not src.
	if got := src.RGBAAt(10, 10); got != (color.RGBA{R: 255, G: 255, B: 255, A: 255}) {
		t.Fatalf("Overlay mutated the source image: %v", got)
	}
}

func TestOverlayClampsOutOfBounds(t *testing.T) {
	src := whiteImage(40, 40)
	// A box partly off-frame must be clamped, not panic; an off-frame box is skipped.
	dst, idbox := Overlay(src, []Mark{
		{ID: 3, Box: image.Rect(30, 30, 80, 80)},     // partly inside
		{ID: 4, Box: image.Rect(100, 100, 120, 120)}, // fully outside
	})
	if _, ok := idbox[3]; !ok {
		t.Fatal("a partly-visible mark should be kept (clamped)")
	}
	if _, ok := idbox[4]; ok {
		t.Fatal("a fully-off-frame mark should be skipped")
	}
	if dst.Bounds() != src.Bounds() {
		t.Fatal("Overlay changed the image bounds")
	}
}

func TestDigitsOf(t *testing.T) {
	cases := map[int][]int{0: {0}, 5: {5}, 27: {2, 7}, 103: {1, 0, 3}}
	for in, want := range cases {
		got := digitsOf(in)
		if len(got) != len(want) {
			t.Fatalf("digitsOf(%d) = %v, want %v", in, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("digitsOf(%d) = %v, want %v", in, got, want)
			}
		}
	}
}
