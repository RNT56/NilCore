package tools

// harden_test.go — I4 read-path regressions for the tools that read an
// already-confined path before writing (edit_checked, format_file, patch). Each now
// reads via readNoFollow/worktreefs.ReadConfined (O_NOFOLLOW), so a final-component
// symlink swapped in AFTER the confinement check — the classic TOCTOU escape — is
// refused rather than followed, and an out-of-worktree secret never reaches the model
// or gets rewritten through the link. These mirror fs_test.go's Read/Edit swap tests.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapFixture creates a worktree dir and an out-of-tree secret, then plants a
// same-named in-tree entry that is actually a symlink to the secret. It returns the
// worktree dir and the secret path; it Skips when symlinks are unsupported.
func swapFixture(t *testing.T, name, secretBody string) (dir, secret string) {
	t.Helper()
	dir = t.TempDir()
	outside := t.TempDir()
	secret = filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte(secretBody), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, name)
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	return dir, secret
}

// TestEditCheckedRefusesFinalComponentSymlinkSwap: edit_checked reads the target via
// O_NOFOLLOW before its syntax gate, so it cannot be tricked into reading (and then
// rewriting through) a symlink swapped in at the final component.
func TestEditCheckedRefusesFinalComponentSymlinkSwap(t *testing.T) {
	dir, secret := swapFixture(t, "target.go", "package secret\n")
	got, err := run(t, EditCheckedTool{}, dir, `{"path":"target.go","old":"secret","new":"x"}`)
	if err == nil {
		t.Fatalf("edit_checked must refuse a final-component symlink; got %q", got)
	}
	if strings.Contains(got, "package secret") {
		t.Fatal("confinement breached: the outside secret leaked through a symlink read")
	}
	if b, rerr := os.ReadFile(secret); rerr != nil || string(b) != "package secret\n" {
		t.Fatalf("confinement breached: outside secret modified (%q, err=%v)", b, rerr)
	}
}

// TestFormatRefusesFinalComponentSymlinkSwap: format_file reads via O_NOFOLLOW, so a
// swapped-in link is refused rather than read and reformatted through.
func TestFormatRefusesFinalComponentSymlinkSwap(t *testing.T) {
	dir, secret := swapFixture(t, "target.go", "package  secret\n")
	got, err := run(t, FormatTool{}, dir, `{"path":"target.go"}`)
	if err == nil {
		t.Fatalf("format_file must refuse a final-component symlink; got %q", got)
	}
	if strings.Contains(got, "secret") {
		t.Fatal("confinement breached: outside secret content surfaced via symlink read")
	}
	if b, rerr := os.ReadFile(secret); rerr != nil || string(b) != "package  secret\n" {
		t.Fatalf("confinement breached: outside secret modified (%q, err=%v)", b, rerr)
	}
}

// TestPatchRefusesFinalComponentSymlinkSwap: patch's update_file reads current bytes
// via O_NOFOLLOW, so a swapped-in link at the target is refused and the outside secret
// is never read or written through.
func TestPatchRefusesFinalComponentSymlinkSwap(t *testing.T) {
	dir, secret := swapFixture(t, "target.txt", "SECRET DATA")
	input := `{"ops":[{"kind":"update_file","path":"target.txt","content":"REPLACED"}]}`
	got, err := run(t, PatchTool{}, dir, input)
	if err == nil {
		t.Fatalf("patch must refuse a final-component symlink read; got %q", got)
	}
	if b, rerr := os.ReadFile(secret); rerr != nil || string(b) != "SECRET DATA" {
		t.Fatalf("confinement breached: outside secret modified (%q, err=%v)", b, rerr)
	}
}
