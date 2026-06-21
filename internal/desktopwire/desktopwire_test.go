package desktopwire

import (
	"encoding/json"
	"testing"
)

func TestBoxCenter(t *testing.T) {
	b := Box{X: 10, Y: 20, W: 100, H: 40}
	x, y := b.Center()
	if x != 60 || y != 40 {
		t.Fatalf("center = (%d,%d), want (60,40)", x, y)
	}
	if b.Empty() {
		t.Fatal("non-zero box reported empty")
	}
	if !(Box{X: 5, Y: 5, W: 0, H: 10}).Empty() {
		t.Fatal("zero-width box should be empty")
	}
}

func TestShellSingleQuote(t *testing.T) {
	// A model string with a quote + shell metachars must stay one quoted token.
	got := ShellSingleQuote("a'; rm -rf / #")
	want := `'a'\''; rm -rf / #'`
	if got != want {
		t.Fatalf("quote = %q, want %q", got, want)
	}
}

func TestObservationRoundTrip(t *testing.T) {
	o := Observation{
		Title: "Settings", FocusedWindow: "gnome-control-center", Version: 3, Rung: RungATSPI,
		Refs: []Ref{{ID: 1, Role: "push button", Name: "Save", Box: Box{X: 1, Y: 2, W: 3, H: 4}, Actions: []string{"click"}}},
	}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var back Observation
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Rung != RungATSPI || len(back.Refs) != 1 || back.Refs[0].Name != "Save" || back.Refs[0].Actions[0] != "click" {
		t.Fatalf("round-trip lost data: %+v", back)
	}
}

func TestActCoordinateOmitempty(t *testing.T) {
	// A ref act must not serialize a coordinate, and vice-versa, so the driver's
	// "ref vs coordinate" branch is unambiguous.
	b, _ := json.Marshal(Act{Op: OpClick, Ref: 5})
	if string(b) != `{"op":"click","ref":5}` {
		t.Fatalf("ref act serialized unexpected fields: %s", b)
	}
	b2, _ := json.Marshal(Act{Op: OpClick, Coordinate: []int{12, 34}})
	if string(b2) != `{"op":"click","coordinate":[12,34]}` {
		t.Fatalf("coordinate act serialized unexpected fields: %s", b2)
	}
}
