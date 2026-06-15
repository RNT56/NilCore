package verb

import (
	"strings"
	"testing"
	"time"
)

// Frame advances every 80ms and loops over the 10-glyph braille cycle.
func TestFrameAdvancesAndLoops(t *testing.T) {
	s := New(1, General)
	if s.Frame(0) != "⠋" {
		t.Errorf("frame@0 = %q, want ⠋", s.Frame(0))
	}
	if s.Frame(80*time.Millisecond) != "⠙" {
		t.Errorf("frame@80ms = %q, want ⠙", s.Frame(80*time.Millisecond))
	}
	// One full cycle (10 frames = 800ms) returns to the first glyph.
	if s.Frame(800*time.Millisecond) != s.Frame(0) {
		t.Error("frame should loop after a full cycle")
	}
}

// Verb is deterministic: the same (seed, elapsed) always yields the same word.
func TestVerbDeterministic(t *testing.T) {
	a := New(42, General)
	for _, d := range []time.Duration{0, 4 * time.Second, 12 * time.Second} {
		if a.Verb(d) != New(42, General).Verb(d) {
			t.Errorf("verb not deterministic at %v", d)
		}
	}
}

// The verb switches across 4s buckets (it is not frozen on one word).
func TestVerbSwitchesOverTime(t *testing.T) {
	s := New(7, General)
	seen := map[string]bool{}
	for b := 0; b < 12; b++ {
		seen[s.Verb(time.Duration(b)*verbEvery)] = true
	}
	if len(seen) < 3 {
		t.Errorf("expected the verb to vary across buckets, saw %d distinct", len(seen))
	}
}

// Each category draws only from its own (or the full) list, and the words are
// present-participles with no trailing ellipsis (the caller adds it).
func TestCategoryBuckets(t *testing.T) {
	for _, c := range []Category{General, Native, Supervise, Project, Chat} {
		s := New(3, c)
		v := s.Verb(8 * time.Second)
		if v == "" || strings.HasSuffix(v, "…") {
			t.Errorf("category %d yielded a bad verb %q", c, v)
		}
		if !contains(byCategory(c), v) {
			t.Errorf("category %d verb %q not from its own list", c, v)
		}
	}
}

// Negative elapsed (clock skew) is clamped, never panics or indexes negatively.
func TestNegativeElapsedClamped(t *testing.T) {
	s := New(1, General)
	if s.Frame(-time.Second) != s.Frame(0) || s.Verb(-time.Second) != s.Verb(0) {
		t.Error("negative elapsed should clamp to 0")
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
