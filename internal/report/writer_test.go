package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolveDir returns an EvalSymlinks-resolved temp dir, so worktreefs's lexical
// containment check (which compares against EvalSymlinks output) lines up on
// platforms where TempDir itself sits under a symlink (e.g. macOS /var ->
// /private/var). Mirrors the worktreefs test helper.
func resolveDir(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return p
}

// TestWriteReport is the named gate (Verify line). It proves the writer lands the
// rendered bytes at the fixed .nilcore/reports/<run>.<ext> path, round-trips
// byte-equal, restricts ext to the closed allowlist, and fails closed on
// path-escape and a planted symlink — owning none of that safety itself, all of it
// delegated to worktreefs.
func TestWriteReport(t *testing.T) {
	t.Run("RoundTrip", testWriteRoundTrip)
	t.Run("ExtAllowlist", testWriteExtAllowlist)
	t.Run("RejectEscape", testWriteRejectEscape)
	t.Run("SymlinkRefused", testWriteSymlinkRefused)
}

// testWriteRoundTrip: a valid write lands at <root>/.nilcore/reports/<run>.<ext>
// and reads back byte-equal to the passed content.
func testWriteRoundTrip(t *testing.T) {
	root := resolveDir(t)
	content := []byte("<html><body>report-body\nline2</body></html>")
	if err := WriteReport(root, "run-001", "html", content); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, ".nilcore", "reports", "run-001.html"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, content)
	}
}

// testWriteExtAllowlist: each of {html,md,txt} is accepted and any other extension
// is rejected without writing a file.
func testWriteExtAllowlist(t *testing.T) {
	root := resolveDir(t)
	for _, ext := range []string{"html", "md", "txt"} {
		if err := WriteReport(root, "ok-"+ext, ext, []byte("x")); err != nil {
			t.Fatalf("ext %q must be allowed: %v", ext, err)
		}
	}
	for _, bad := range []string{"", "json", "exe", "sh", "HTML", "html.txt"} {
		if err := WriteReport(root, "bad", bad, []byte("x")); err == nil {
			t.Fatalf("ext %q must be rejected", bad)
		}
		if _, err := os.Stat(filepath.Join(root, ".nilcore", "reports", "bad."+bad)); err == nil {
			t.Fatalf("ext %q rejected but a file was written", bad)
		}
	}
}

// testWriteRejectEscape: a run with a separator, a "..", or a leading dot is
// refused before any byte is written, so no escape file is created.
func testWriteRejectEscape(t *testing.T) {
	root := resolveDir(t)
	for _, run := range []string{"", ".", "..", "a/b", "../escape", ".hidden", "sub" + string(os.PathSeparator) + "x"} {
		if err := WriteReport(root, run, "txt", []byte("x")); err == nil {
			t.Fatalf("run %q must be rejected", run)
		}
	}
	// An attempt to climb out of the reports dir must not have created anything
	// outside it.
	parent := filepath.Dir(root)
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
		t.Fatal("confinement breached: an escape file was created outside root")
	}
}

// testWriteSymlinkRefused: a symlink planted at the destination that points outside
// root is refused at the worktreefs confinement step, so the write fails closed and
// the outside secret is untouched.
func testWriteSymlinkRefused(t *testing.T) {
	root := resolveDir(t)
	outside := resolveDir(t)
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".nilcore", "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, ".nilcore", "reports", "run.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := WriteReport(root, "run", "txt", []byte("NEW")); err == nil {
		t.Fatal("WriteReport must refuse a target whose symlink escapes root")
	}
	got, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ORIGINAL" {
		t.Fatal("confinement breached: the outside secret was overwritten through a symlink")
	}
}

// Guard: keep the strings import meaningful only if used; the allowlist error
// message is human-checked, not asserted on substring, so we avoid an unused
// import by referencing strings here in a cheap sanity check that the allowlist
// keys are bare suffixes (no dot/separator).
func TestWriteReportAllowlistShape(t *testing.T) {
	for ext := range allowedExts {
		if strings.ContainsAny(ext, "./\\") {
			t.Fatalf("allowlist ext %q must be a bare suffix", ext)
		}
	}
}
