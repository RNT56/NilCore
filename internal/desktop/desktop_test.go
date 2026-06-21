package desktop

import (
	"image"
	"image/color"
	"testing"

	"nilcore/internal/desktopwire"
)

func TestLadderRungSelection(t *testing.T) {
	l := NewLadder()

	// Rich tree → Rung 1.
	if d := l.Decide(RungInput{Window: "settings", RefCount: 12, HasMarkableBoxes: true}); d.Rung != desktopwire.RungATSPI {
		t.Fatalf("rich tree → rung %d, want 1 (%s)", d.Rung, d.Reason)
	}
	// Empty tree but markable → Rung 2.
	if d := l.Decide(RungInput{Window: "canvasapp", RefCount: 0, HasMarkableBoxes: true}); d.Rung != desktopwire.RungSoM {
		t.Fatalf("empty+markable → rung %d, want 2", d.Rung)
	}
	// Nothing → Rung 3.
	if d := l.Decide(RungInput{Window: "game", RefCount: 0, HasMarkableBoxes: false}); d.Rung != desktopwire.RungCoordinate {
		t.Fatalf("nothing → rung %d, want 3", d.Rung)
	}
}

func TestLadderStagnationDowngrades(t *testing.T) {
	l := NewLadder()
	// First observation of a window with a tree → Rung 1.
	if d := l.Decide(RungInput{Window: "electron", RefCount: 5, HasMarkableBoxes: true}); d.Rung != desktopwire.RungATSPI {
		t.Fatalf("want rung 1 initially, got %d", d.Rung)
	}
	// A ref-click verifiably did nothing → the tree is lying; drop to Rung 2 even
	// though refs are still reported.
	if d := l.Decide(RungInput{Window: "electron", RefCount: 5, HasMarkableBoxes: true, Stagnant: true}); d.Rung != desktopwire.RungSoM {
		t.Fatalf("stagnation should downgrade to rung 2, got %d (%s)", d.Rung, d.Reason)
	}
	// It stays downgraded for the same window.
	if d := l.Decide(RungInput{Window: "electron", RefCount: 5, HasMarkableBoxes: true}); d.Rung != desktopwire.RungSoM {
		t.Fatalf("should remain downgraded, got %d", d.Rung)
	}
	// Focus change re-probes fresh → Rung 1 again.
	if d := l.Decide(RungInput{Window: "settings", RefCount: 5, HasMarkableBoxes: true}); d.Rung != desktopwire.RungATSPI {
		t.Fatalf("new window should re-probe to rung 1, got %d", d.Rung)
	}
}

func TestLadderInvalidate(t *testing.T) {
	l := NewLadder()
	l.Decide(RungInput{Window: "w", RefCount: 3, HasMarkableBoxes: true, Stagnant: true}) // downgrades w
	l.Invalidate("w")                                                                     // e.g. a resize
	// After invalidation the same window re-probes fresh.
	if d := l.Decide(RungInput{Window: "w", RefCount: 3, HasMarkableBoxes: true}); d.Rung != desktopwire.RungATSPI {
		t.Fatalf("after Invalidate, want a fresh rung 1, got %d", d.Rung)
	}
}

func TestDetectFindsBoxes(t *testing.T) {
	// A blank canvas with two filled dark rectangles → detect should find ~2 regions
	// roughly bounding them.
	img := image.NewRGBA(image.Rect(0, 0, 200, 150))
	for y := 0; y < 150; y++ {
		for x := 0; x < 200; x++ {
			img.Set(x, y, color.White)
		}
	}
	fill := func(r image.Rectangle) {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				img.Set(x, y, color.RGBA{R: 20, G: 20, B: 20, A: 255})
			}
		}
	}
	fill(image.Rect(20, 20, 70, 50))
	fill(image.Rect(120, 90, 180, 130))

	boxes := Detect(img, DefaultDetectOptions())
	if len(boxes) < 2 {
		t.Fatalf("expected at least 2 detected boxes, got %d: %v", len(boxes), boxes)
	}
	// Each filled rect should be roughly covered by some detected box.
	for _, target := range []image.Rectangle{image.Rect(20, 20, 70, 50), image.Rect(120, 90, 180, 130)} {
		tcx, tcy := (target.Min.X+target.Max.X)/2, (target.Min.Y+target.Max.Y)/2
		covered := false
		for _, bx := range boxes {
			if image.Pt(tcx, tcy).In(bx) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("no detected box covers the centre of %v; boxes=%v", target, boxes)
		}
	}
}
