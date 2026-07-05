package maint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCollect(t *testing.T) {
	tests := []struct {
		name      string
		items     []Item
		failOn    map[string]bool // IDs whose Remove returns an error
		want      []string        // expected removed (in order)
		wantTried []string        // expected Remove call order
		wantErr   bool
	}{
		{
			name: "removes stale, preserves active",
			items: []Item{
				{ID: "a", Active: false},
				{ID: "b", Active: true},
				{ID: "c", Active: false},
			},
			want:      []string{"a", "c"},
			wantTried: []string{"a", "c"},
		},
		{
			name: "all active removes nothing",
			items: []Item{
				{ID: "a", Active: true},
				{ID: "b", Active: true},
			},
			want:      nil,
			wantTried: nil,
		},
		{
			name:      "empty list",
			items:     nil,
			want:      nil,
			wantTried: nil,
		},
		{
			name: "remove error on one does not stop the rest",
			items: []Item{
				{ID: "a", Active: false},
				{ID: "b", Active: false},
				{ID: "c", Active: false},
			},
			failOn:    map[string]bool{"b": true},
			want:      []string{"a", "c"},
			wantTried: []string{"a", "b", "c"},
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tried []string
			g := GC{
				List: func() ([]Item, error) { return tt.items, nil },
				Remove: func(id string) error {
					tried = append(tried, id)
					if tt.failOn[id] {
						return errors.New("boom")
					}
					return nil
				},
			}

			removed, err := g.Collect(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(removed, tt.want) {
				t.Errorf("removed = %v, want %v", removed, tt.want)
			}
			if !reflect.DeepEqual(tried, tt.wantTried) {
				t.Errorf("Remove call order = %v, want %v", tried, tt.wantTried)
			}
		})
	}
}

func TestCollectListError(t *testing.T) {
	sentinel := errors.New("list failed")
	g := GC{
		List:   func() ([]Item, error) { return nil, sentinel },
		Remove: func(string) error { t.Fatal("Remove must not be called when List fails"); return nil },
	}
	removed, err := g.Collect(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped %v", err, sentinel)
	}
	if removed != nil {
		t.Errorf("removed = %v, want nil", removed)
	}
}

func TestCollectNilSeams(t *testing.T) {
	if _, err := (GC{Remove: func(string) error { return nil }}).Collect(context.Background()); err == nil {
		t.Error("nil List: want error, got nil")
	}
	if _, err := (GC{List: func() ([]Item, error) { return nil, nil }}).Collect(context.Background()); err == nil {
		t.Error("nil Remove: want error, got nil")
	}
}

func TestCollectCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before the first item

	called := false
	g := GC{
		List:   func() ([]Item, error) { return []Item{{ID: "a"}}, nil },
		Remove: func(string) error { called = true; return nil },
	}
	removed, err := g.Collect(ctx)
	if err == nil {
		t.Fatal("want cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want wrapped context.Canceled", err)
	}
	if called {
		t.Error("Remove should not run after cancellation")
	}
	if removed != nil {
		t.Errorf("removed = %v, want nil", removed)
	}
}

func TestRotateLog(t *testing.T) {
	t.Run("rotates over-size file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "events.log")
		body := []byte("0123456789ABCDEF") // 16 bytes
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("seeding log: %v", err)
		}

		if err := RotateLog(path, 8); err != nil {
			t.Fatalf("RotateLog: %v", err)
		}

		// Live path exists and is now empty.
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat live path: %v", err)
		}
		if info.Size() != 0 {
			t.Errorf("live size = %d, want 0", info.Size())
		}
		// Rotated copy holds the original bytes.
		got, err := os.ReadFile(path + ".1")
		if err != nil {
			t.Fatalf("read rotated: %v", err)
		}
		if !reflect.DeepEqual(got, body) {
			t.Errorf("rotated content = %q, want %q", got, body)
		}
	})

	t.Run("leaves small file alone", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "events.log")
		body := []byte("tiny")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("seeding log: %v", err)
		}

		if err := RotateLog(path, 1024); err != nil {
			t.Fatalf("RotateLog: %v", err)
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read live path: %v", err)
		}
		if !reflect.DeepEqual(got, body) {
			t.Errorf("content = %q, want %q (file must be untouched)", got, body)
		}
		if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
			t.Errorf("rotated file should not exist, stat err = %v", err)
		}
	})

	t.Run("at exactly the cap is left alone", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "events.log")
		if err := os.WriteFile(path, []byte("12345678"), 0o644); err != nil { // 8 bytes
			t.Fatalf("seeding log: %v", err)
		}
		if err := RotateLog(path, 8); err != nil {
			t.Fatalf("RotateLog: %v", err)
		}
		if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
			t.Errorf("must not rotate at exactly the cap, stat err = %v", err)
		}
	})

	t.Run("missing file is a no-op", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "nope.log")
		if err := RotateLog(path, 8); err != nil {
			t.Fatalf("RotateLog on missing file: %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("RotateLog must not create a missing file, stat err = %v", err)
		}
	})

	t.Run("negative maxBytes errors", func(t *testing.T) {
		if err := RotateLog(filepath.Join(t.TempDir(), "x.log"), -1); err == nil {
			t.Error("want error for negative maxBytes, got nil")
		}
	})

	// Rotation must be LOSSLESS (I5): a second rotation must NOT overwrite the first
	// generation. ".1" holds the newest rotation, ".2" the older one — both survive.
	t.Run("two rotations preserve both generations", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "events.log")
		gen1Body := []byte("FIRST-GENERATION-0123456789") // >8 bytes so it rotates

		// First fill + rotate: this content becomes generation .1.
		if err := os.WriteFile(path, gen1Body, 0o644); err != nil {
			t.Fatalf("seed gen1: %v", err)
		}
		if err := RotateLog(path, 8); err != nil {
			t.Fatalf("first RotateLog: %v", err)
		}

		// Second fill + rotate: DIFFERENT content. gen1 must cascade .1 -> .2, and the
		// new content lands in .1. Nothing is destroyed.
		gen2Body := []byte("SECOND-GENERATION-ABCDEFGHIJ")
		if err := os.WriteFile(path, gen2Body, 0o644); err != nil {
			t.Fatalf("seed gen2: %v", err)
		}
		if err := RotateLog(path, 8); err != nil {
			t.Fatalf("second RotateLog: %v", err)
		}

		// .1 is the newest rotation (gen2); .2 is the older one (gen1) — both intact.
		got1, err := os.ReadFile(path + ".1")
		if err != nil {
			t.Fatalf("read .1: %v", err)
		}
		if !reflect.DeepEqual(got1, gen2Body) {
			t.Errorf(".1 = %q, want newest generation %q", got1, gen2Body)
		}
		got2, err := os.ReadFile(path + ".2")
		if err != nil {
			t.Fatalf("read .2 (must survive the second rotation): %v", err)
		}
		if !reflect.DeepEqual(got2, gen1Body) {
			t.Errorf(".2 = %q, want the preserved first generation %q", got2, gen1Body)
		}

		// A third rotation cascades again: .2 -> .3, .1 -> .2, new -> .1. All three live.
		gen3Body := []byte("THIRD-GENERATION-KLMNOPQRST")
		if err := os.WriteFile(path, gen3Body, 0o644); err != nil {
			t.Fatalf("seed gen3: %v", err)
		}
		if err := RotateLog(path, 8); err != nil {
			t.Fatalf("third RotateLog: %v", err)
		}
		for suffix, want := range map[string][]byte{".1": gen3Body, ".2": gen2Body, ".3": gen1Body} {
			got, err := os.ReadFile(path + suffix)
			if err != nil {
				t.Fatalf("read %s: %v", suffix, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s = %q, want %q", suffix, got, want)
			}
		}
	})
}
