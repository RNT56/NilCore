package main

import (
	"testing"

	"nilcore/internal/blastbudget"
	"nilcore/internal/sandbox"
)

// TestAttachBlast proves the BR-T04 sandbox-threading seam: a nil budget leaves the box
// UNCHANGED (default-off byte-identical), and a real budget is attached to a container
// box so its wall-time axis is fenced.
func TestAttachBlast(t *testing.T) {
	c := sandbox.NewContainer("docker", "img", t.TempDir())
	var box sandbox.Sandbox = c

	// nil budget ⇒ unchanged, Blast stays nil (an unfenced run is byte-identical).
	if got := attachBlast(box, nil); got != box || c.Blast != nil {
		t.Fatal("a nil budget must leave the box unchanged")
	}

	// A real budget is attached to the container.
	b := blastbudget.New()
	if got := attachBlast(box, b); got != box || c.Blast != b {
		t.Fatal("a budget must be attached to a container box")
	}
}
