package maint

import (
	"context"
	"errors"
	"testing"
)

// capOver builds a Cap over a fixed newest-first item list, recording removals.
func capOver(items []Item, keep int, removeErr map[string]error) (Cap, *[]string) {
	var removed []string
	c := Cap{
		Keep: keep,
		List: func() ([]Item, error) { return items, nil },
		Remove: func(id string) error {
			if err := removeErr[id]; err != nil {
				return err
			}
			removed = append(removed, id)
			return nil
		},
	}
	return c, &removed
}

// names builds n inactive items named b1..bn, NEWEST-FIRST (b1 is newest).
func names(n int) []Item {
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{ID: "b" + string(rune('0'+((i+1)/10))) + string(rune('0'+((i+1)%10)))}
	}
	return items
}

// TestCapCollect is the acceptance case: N+3 kept branches with a cap of N prune
// exactly the 3 OLDEST (the tail of the newest-first list), newest N untouched.
func TestCapCollect(t *testing.T) {
	const keep = 20
	items := names(keep + 3)
	c, removed := capOver(items, keep, nil)

	got, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != 3 || len(*removed) != 3 {
		t.Fatalf("removed %v, want the 3 oldest", got)
	}
	for i, want := range []string{items[keep].ID, items[keep+1].ID, items[keep+2].ID} {
		if got[i] != want {
			t.Errorf("removed[%d] = %q, want %q (oldest-past-cap in list order)", i, got[i], want)
		}
	}
}

func TestCapCollectUnderBudgetRemovesNothing(t *testing.T) {
	c, removed := capOver(names(5), 20, nil)
	got, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != 0 || len(*removed) != 0 {
		t.Fatalf("removed %v from an under-budget family, want none", got)
	}
}

// An Active item past the cap boundary is never removed — but it still consumes a
// slot, so the retained set stays bounded.
func TestCapCollectSkipsActive(t *testing.T) {
	items := []Item{
		{ID: "new1"}, {ID: "new2"},
		{ID: "live", Active: true}, // older than the cap would allow, but live
		{ID: "old1"}, {ID: "old2"},
	}
	c, _ := capOver(items, 3, nil)
	got, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// Budget 3 = new1 + new2 + live (active counts); old1/old2 pruned.
	if len(got) != 2 || got[0] != "old1" || got[1] != "old2" {
		t.Fatalf("removed %v, want [old1 old2] (active kept, budget respected)", got)
	}
}

// Collect-and-continue: one failed removal is reported but the rest still prune.
func TestCapCollectContinuesPastRemoveError(t *testing.T) {
	boom := errors.New("boom")
	items := []Item{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	c, _ := capOver(items, 1, map[string]error{"b": boom})
	got, err := c.Collect(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want joined boom", err)
	}
	if len(got) != 1 || got[0] != "c" {
		t.Fatalf("removed %v, want [c] (b failed, c still attempted)", got)
	}
}

func TestCapCollectNilSeams(t *testing.T) {
	if _, err := (Cap{Keep: 1, Remove: func(string) error { return nil }}).Collect(context.Background()); err == nil {
		t.Error("nil List must error")
	}
	if _, err := (Cap{Keep: 1, List: func() ([]Item, error) { return nil, nil }}).Collect(context.Background()); err == nil {
		t.Error("nil Remove must error")
	}
}

// A cancelled ctx stops the sweep between items (no further removals attempted).
func TestCapCollectHonorsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, removed := capOver(names(4), 1, nil)
	if _, err := c.Collect(ctx); err == nil {
		t.Fatal("cancelled Collect must report the cancellation")
	}
	if len(*removed) != 0 {
		t.Fatalf("cancelled Collect removed %v, want none", *removed)
	}
}
