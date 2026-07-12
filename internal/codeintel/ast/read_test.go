package ast

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestReadGuardsSkipSymlinkedFile is the I4 regression: filepath.WalkDir does not
// descend a symlinked DIRECTORY but it DOES yield a symlinked FILE, and a plain os.Open
// (or go/parser with a nil src) would then follow it and read out-of-worktree bytes into
// the index. The O_NOFOLLOW opener must fail-closed on such a link so every dispatcher
// (Symbols/References/Calls) skips it cleanly — its target's symbols must NEVER surface.
//
// Both read paths are covered: .go exercises readSource (go/parser), .py exercises
// openSource (the streaming scanner backends).
func TestReadGuardsSkipSymlinkedFile(t *testing.T) {
	cases := []struct {
		name       string
		linkName   string
		target     string // out-of-worktree source content
		leakSymbol string // a symbol that appears ONLY in the target
	}{
		{
			name:       "go",
			linkName:   "evil.go",
			target:     "package secret\nfunc LeakedFromOutsideWorktree() {}\n",
			leakSymbol: "LeakedFromOutsideWorktree",
		},
		{
			name:       "python",
			linkName:   "evil.py",
			target:     "def leaked_from_outside_worktree():\n    pass\n",
			leakSymbol: "leaked_from_outside_worktree",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A secret file OUTSIDE the worktree.
			secret := filepath.Join(t.TempDir(), "secret"+filepath.Ext(tc.linkName))
			if err := os.WriteFile(secret, []byte(tc.target), 0o644); err != nil {
				t.Fatal(err)
			}
			// The worktree holds ONLY a source-looking symlink pointing at the secret.
			worktree := t.TempDir()
			link := filepath.Join(worktree, tc.linkName)
			if err := os.Symlink(secret, link); err != nil {
				t.Skipf("symlinks unsupported on this platform: %v", err)
			}

			// Every dispatcher must skip the symlink cleanly (nil error, no output) — the
			// symlink is refused at open, not followed.
			syms, err := Symbols(link)
			if err != nil {
				t.Errorf("Symbols on symlink: want clean skip, got err %v", err)
			}
			for _, s := range syms {
				if s.Name == tc.leakSymbol {
					t.Fatalf("symlink was followed: leaked symbol %q entered the index", tc.leakSymbol)
				}
			}
			if refs, err := References(link); err != nil {
				t.Errorf("References on symlink: want clean skip, got err %v", err)
			} else if len(refs) != 0 {
				t.Errorf("References on symlink: want none, got %d", len(refs))
			}
			if calls, err := Calls(link); err != nil {
				t.Errorf("Calls on symlink: want clean skip, got err %v", err)
			} else if len(calls) != 0 {
				t.Errorf("Calls on symlink: want none, got %d", len(calls))
			}

			// And through the actual index walk: WalkDir yields the symlinked file; the
			// parser must not read its out-of-tree target.
			err = filepath.WalkDir(worktree, func(path string, d fs.DirEntry, werr error) error {
				if werr != nil || d.IsDir() {
					return werr
				}
				got, serr := Symbols(path)
				if serr != nil {
					t.Errorf("walk: Symbols(%s) errored instead of skipping: %v", path, serr)
				}
				for _, s := range got {
					if s.Name == tc.leakSymbol {
						t.Fatalf("walk followed the symlink: %q leaked into the index", tc.leakSymbol)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestReadGuardsSkipOversizedFile is the DoS regression: a single crafted file larger
// than the per-file cap must be skipped BEFORE it is read, so it can never OOM the host.
// The file carries a valid symbol at its head, so if the cap failed to skip it the symbol
// would appear — it must not. Covers both read paths (.go via readSource, .py via
// openSource).
func TestReadGuardsSkipOversizedFile(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		head    string // valid source declaring `symbol`, padded past the cap below
		symbol  string
		padByte byte
	}{
		{name: "go", file: "big.go", head: "package x\nfunc TooBigToIndex() {}\n", symbol: "TooBigToIndex", padByte: '\n'},
		{name: "python", file: "big.py", head: "def too_big_to_index():\n    pass\n", symbol: "too_big_to_index", padByte: '\n'},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, tc.file)
			// head + enough blank lines to push total size to maxFileBytes+1 (strictly
			// over the cap). Blank-line padding keeps the file syntactically valid, so the
			// ONLY reason the symbol goes unseen is the size skip, not a parse failure.
			content := append([]byte(tc.head), bytes.Repeat([]byte{tc.padByte}, maxFileBytes+1-len(tc.head))...)
			if len(content) <= maxFileBytes {
				t.Fatalf("test setup: content %d bytes is not over the cap %d", len(content), maxFileBytes)
			}
			if err := os.WriteFile(p, content, 0o644); err != nil {
				t.Fatal(err)
			}
			syms, err := Symbols(p)
			if err != nil {
				t.Fatalf("oversized file: want clean skip, got err %v", err)
			}
			for _, s := range syms {
				if s.Name == tc.symbol {
					t.Fatalf("oversized file was parsed instead of skipped (symbol %q surfaced)", tc.symbol)
				}
			}
			if len(syms) != 0 {
				t.Errorf("oversized file: want no symbols, got %d", len(syms))
			}
		})
	}
}

// TestReadGuardsIndexFileAtSizeCap is the accept-direction boundary: the cap is a
// strict `>` bound, so a file of EXACTLY maxFileBytes bytes is still read. This proves
// the DoS guard does not over-reject ordinary files right at the limit.
func TestReadGuardsIndexFileAtSizeCap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "atcap.go")
	head := "package x\nfunc AtCap() {}\n"
	content := append([]byte(head), bytes.Repeat([]byte{'\n'}, maxFileBytes-len(head))...)
	if len(content) != maxFileBytes {
		t.Fatalf("test setup: content %d bytes, want exactly %d", len(content), maxFileBytes)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	syms, err := Symbols(p)
	if err != nil {
		t.Fatalf("at-cap file should be read, got err %v", err)
	}
	var found bool
	for _, s := range syms {
		if s.Name == "AtCap" {
			found = true
		}
	}
	if !found {
		t.Error("a file exactly at the size cap should still be indexed, but AtCap was not found")
	}
}
