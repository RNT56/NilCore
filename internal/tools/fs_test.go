package tools

// Tests for the bounded, pageable read (offset/limit windows + the maxReadBytes
// cap). The plumbing-level read/write/edit tests stay in tools_test.go; this file
// proves the context-flooding guard: a huge file comes back as a head plus a
// harness notice that teaches the offset-based recovery move, and an explicit
// window returns exactly what was asked for — still under the cap.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// fiveLines is a small fixture with a final trailing newline (the common file shape).
const fiveLines = "l1\nl2\nl3\nl4\nl5\n"

func writeFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// truncNoticeRE matches the exact notice the tool description promises the model.
var truncNoticeRE = regexp.MustCompile(`\[truncated at line (\d+) of (\d+) total lines — re-read with offset=(\d+) to continue\]$`)

// bigFixture writes a file of 600 numbered 100-byte lines (~60KB, over
// maxReadBytes) and returns the lines, so tests can verify exact head content.
func bigFixture(t *testing.T, dir string) []string {
	t.Helper()
	lines := make([]string, 600)
	for i := range lines {
		lines[i] = fmt.Sprintf("L%04d %s", i+1, strings.Repeat("x", 93)) // 99 bytes + newline
	}
	writeFixture(t, dir, "big.txt", strings.Join(lines, "\n")+"\n")
	return lines
}

func TestReadSmallFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "a.txt", fiveLines)
	got, err := run(t, ReadTool{}, dir, `{"path":"a.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	// Byte-identical, trailing newline included: the default read of a small file
	// must behave exactly as it always has.
	if got != fiveLines {
		t.Fatalf("small file must pass through byte-identical: %q", got)
	}
}

func TestReadWindows(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "a.txt", fiveLines)

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			"window at start",
			`{"path":"a.txt","offset":1,"limit":2}`,
			"l1\nl2\n[showing lines 1-2 of 5 total lines — re-read with offset=3 to continue]",
		},
		{
			"window in the middle",
			`{"path":"a.txt","offset":3,"limit":2}`,
			"l3\nl4\n[showing lines 3-4 of 5 total lines — re-read with offset=5 to continue]",
		},
		{
			"offset to EOF needs no notice",
			`{"path":"a.txt","offset":4}`,
			"l4\nl5",
		},
		{
			"limit alone defaults offset to line 1",
			`{"path":"a.txt","limit":2}`,
			"l1\nl2\n[showing lines 1-2 of 5 total lines — re-read with offset=3 to continue]",
		},
		{
			"offset past EOF is clean and says so",
			`{"path":"a.txt","offset":99}`,
			"[no content: offset 99 is past end of file — 5 total lines]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := run(t, ReadTool{}, dir, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestReadRejectsNegativeWindow(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "a.txt", fiveLines)
	for _, in := range []string{`{"path":"a.txt","offset":-1}`, `{"path":"a.txt","limit":-2}`} {
		if _, err := run(t, ReadTool{}, dir, in); err == nil {
			t.Errorf("expected an error for %s", in)
		}
	}
}

func TestReadOversizedTruncatedWithRecoveryNotice(t *testing.T) {
	dir := t.TempDir()
	lines := bigFixture(t, dir)

	got, err := run(t, ReadTool{}, dir, `{"path":"big.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	m := truncNoticeRE.FindStringSubmatch(got)
	if m == nil {
		tail := got
		if len(tail) > 200 {
			tail = tail[len(tail)-200:]
		}
		t.Fatalf("oversized read must end with the truncation notice; tail: %q", tail)
	}
	if m[2] != "600" {
		t.Errorf("notice must name the true total (600 lines), got %s", m[2])
	}
	if m[1] != m[3] {
		t.Errorf("truncation line and continue offset must agree: %s vs %s", m[1], m[3])
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimSuffix(got, "\n"+m[0])
	if want := strings.Join(lines[:n-1], "\n"); body != want {
		t.Errorf("body must be exactly lines 1..N-1 (N=%d): got %d bytes, want %d", n, len(body), len(want))
	}
	if len(body) > maxReadBytes {
		t.Errorf("body exceeds the byte cap: %d > %d", len(body), maxReadBytes)
	}

	// The notice must teach a WORKING recovery move: offset=N continues at line N.
	next, err := run(t, ReadTool{}, dir, fmt.Sprintf(`{"path":"big.txt","offset":%d}`, n))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(next, lines[n-1]) {
		t.Errorf("re-read with offset=%d must continue at line %d", n, n)
	}
}

func TestReadExplicitWindowStillByteCapped(t *testing.T) {
	dir := t.TempDir()
	bigFixture(t, dir)
	got, err := run(t, ReadTool{}, dir, `{"path":"big.txt","offset":1,"limit":600}`)
	if err != nil {
		t.Fatal(err)
	}
	if !truncNoticeRE.MatchString(got) {
		t.Error("a cap-cut explicit window must carry the truncation notice")
	}
	// Content stays under the cap; only the one-line notice rides on top.
	if len(got) > maxReadBytes+200 {
		t.Errorf("windowed read exceeds the cap: %d bytes", len(got))
	}
}

func TestReadClipsSingleOversizedLine(t *testing.T) {
	dir := t.TempDir()
	// A single line longer than the whole cap (minified-file shape): the clip must
	// keep pagination progressing to line 2 instead of looping on line 1 forever.
	writeFixture(t, dir, "min.js", strings.Repeat("y", maxReadBytes+1000)+"\nrest\n")
	got, err := run(t, ReadTool{}, dir, `{"path":"min.js"}`)
	if err != nil {
		t.Fatal(err)
	}
	m := truncNoticeRE.FindStringSubmatch(got)
	if m == nil {
		t.Fatal("clipped line must still carry the truncation notice")
	}
	if m[1] != "2" || m[2] != "2" {
		t.Errorf("paging must progress past the clipped line: N=%s M=%s, want N=2 M=2", m[1], m[2])
	}
	body := strings.TrimSuffix(got, "\n"+m[0])
	if len(body) != maxReadBytes {
		t.Errorf("clipped body = %d bytes, want exactly %d", len(body), maxReadBytes)
	}
}

// TestReadRefusesFinalComponentSymlinkSwap is the adversarial regression for the
// read-side TOCTOU (I4): a sandboxed process swaps an in-tree file name for a symlink
// pointing at an out-of-worktree secret after the confinement check. ReadTool now
// opens with O_NOFOLLOW (via worktreefs.ReadConfined), so the swapped-in link is
// refused and the secret's bytes never reach the model.
func TestReadRefusesFinalComponentSymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A same-named file swapped for a symlink to the outside secret.
	link := filepath.Join(dir, "innocent.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	got, err := run(t, ReadTool{}, dir, `{"path":"innocent.txt"}`)
	if err == nil {
		t.Fatalf("ReadTool must refuse a final-component symlink; got %q", got)
	}
	if strings.Contains(got, "TOP SECRET") {
		t.Fatal("confinement breached: the outside secret leaked through a symlink read")
	}
}

// TestEditRefusesFinalComponentSymlinkSwap: EditTool likewise reads via O_NOFOLLOW,
// so it cannot be tricked into reading (and then rewriting through) a symlink swapped
// in at the final component after the confinement check.
func TestEditRefusesFinalComponentSymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET DATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "target.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := run(t, EditTool{}, dir, `{"path":"target.txt","old":"SECRET","new":"X"}`); err == nil {
		t.Fatal("EditTool must refuse a final-component symlink read")
	}
	// The outside secret must be untouched.
	got, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "SECRET DATA" {
		t.Fatal("confinement breached: the outside secret was modified through a symlink")
	}
}

func TestReadDescriptionTeachesPaging(t *testing.T) {
	// The description is how the model learns the recovery move — both variants
	// (worktree-only and with read roots) must teach offset/limit + the marker.
	for _, tool := range []ReadTool{{}, {ReadRoots: []string{"/x"}}} {
		d := tool.Description()
		for _, want := range []string{"offset", "limit", "truncated at line"} {
			if !strings.Contains(d, want) {
				t.Errorf("description (roots=%d) must mention %q: %s", len(tool.ReadRoots), want, d)
			}
		}
	}
}
