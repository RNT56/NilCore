//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// homeMasks lists the expected home-relative mask targets, mirroring the
// curated set in buildMaskSet (kept in lockstep by TestBuildMaskSet/full-set).
func homeMasks(home string) []string {
	return []string{
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".aws"),
		filepath.Join(home, ".gnupg"),
		filepath.Join(home, ".netrc"),
		filepath.Join(home, ".git-credentials"),
		filepath.Join(home, ".config", "git", "credentials"),
		filepath.Join(home, ".config", "gh"),
		filepath.Join(home, ".docker", "config.json"),
		filepath.Join(home, ".codex"),
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".claude.json"),
	}
}

func TestBuildMaskSet(t *testing.T) {
	const home = "/home/op"
	const cfg = "/home/op/.config/nilcore"

	// without drops every element of want containing s (used to express "the
	// full set minus the workdir-overlapping targets").
	without := func(want []string, s string) []string {
		out := make([]string, 0, len(want))
		for _, w := range want {
			if w != s {
				out = append(out, w)
			}
		}
		return out
	}
	full := append([]string{cfg}, homeMasks(home)...)

	tests := []struct {
		name               string
		home, cfg, workdir string
		want               []string
	}{
		{
			name: "full set",
			home: home, cfg: cfg, workdir: "/tmp/nilcore-wt-1/repo",
			want: full,
		},
		{
			name: "workdir inside a target drops exactly that target",
			home: home, cfg: cfg, workdir: "/home/op/.claude/worktrees/wt",
			want: without(full, "/home/op/.claude"),
		},
		{
			name: "workdir equal to a target drops it",
			home: home, cfg: cfg, workdir: "/home/op/.aws",
			want: without(full, "/home/op/.aws"),
		},
		{
			name: "targets inside the workdir are dropped (worktree = home)",
			home: home, cfg: "/etc/xdg/nilcore", workdir: home,
			want: []string{"/etc/xdg/nilcore"},
		},
		{
			name: "config dir colliding with a home target is deduped",
			home: home, cfg: "/home/op/.claude", workdir: "/tmp/wt",
			// cfg comes first, so .claude appears once, in front.
			want: append([]string{"/home/op/.claude"}, without(homeMasks(home), "/home/op/.claude")...),
		},
		{
			name: "empty home masks only the config dir",
			home: "", cfg: cfg, workdir: "/tmp/wt",
			want: []string{cfg},
		},
		{
			name: "empty config dir masks only the home set",
			home: home, cfg: "", workdir: "/tmp/wt",
			want: homeMasks(home),
		},
		{
			name: "nothing resolvable masks nothing",
			home: "", cfg: "", workdir: "/tmp/wt",
			want: []string{},
		},
		{
			name: "unclean inputs are cleaned before comparison",
			home: "/home/op/", cfg: "/home/op//.config/nilcore/", workdir: "/tmp/wt/../wt",
			want: full,
		},
		{
			name: "relative home yields no relative masks",
			home: "op-home", cfg: cfg, workdir: "/tmp/wt",
			want: []string{cfg},
		},
		{
			name: "empty workdir disables the overlap exclusion only",
			home: home, cfg: cfg, workdir: "",
			want: full,
		},
		{
			name: "workdir at / excludes everything (degenerate, never shipped)",
			home: home, cfg: cfg, workdir: "/",
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildMaskSet(tt.home, tt.cfg, tt.workdir)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildMaskSet(%q, %q, %q):\n got  %v\n want %v",
					tt.home, tt.cfg, tt.workdir, got, tt.want)
			}
		})
	}
}

func TestPathContains(t *testing.T) {
	tests := []struct {
		parent, child string
		want          bool
	}{
		{"/a", "/a", true},
		{"/a", "/a/b", true},
		{"/a", "/ab", false}, // prefix of the name, not of the path
		{"/a/b", "/a", false},
		{"/", "/anything", true},
		{"/home/op/.ssh", "/home/op/.ssh/id_ed25519", true},
	}
	for _, tt := range tests {
		if got := pathContains(tt.parent, tt.child); got != tt.want {
			t.Errorf("pathContains(%q, %q) = %v, want %v", tt.parent, tt.child, got, tt.want)
		}
	}
}

// TestBuildMaskSetSymlinkOverlap covers FIX #20: a candidate that only overlaps
// the worktree AFTER symlink resolution must still be excluded. We build a real
// symlink (linkHome -> realHome) and place the worktree under realHome/.claude;
// buildMaskSet is called with the SYMLINK path for the .claude candidate but the
// already-resolved worktree, so a purely lexical prefix check would miss the
// overlap and mask the worktree. resolveForOverlap must catch it.
func TestBuildMaskSetSymlinkOverlap(t *testing.T) {
	realHome := t.TempDir()
	linkHome := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(realHome, linkHome); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Worktree lives under the REAL home's .claude; give the parent its resolved form.
	wt := filepath.Join(realHome, ".claude", "worktrees", "wt")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	// Home is passed as the SYMLINK path, so .claude resolves to realHome/.claude
	// only via resolveForOverlap — a lexical check on linkHome/.claude would not
	// prefix-match the resolved worktree.
	got := buildMaskSet(linkHome, "", wt)
	claudeReal := filepath.Join(realHome, ".claude")
	for _, m := range got {
		if m == claudeReal || strings.HasPrefix(m, claudeReal+"/") {
			t.Fatalf("worktree-overlapping .claude must be excluded after symlink resolution, got %v", got)
		}
	}
	// A sibling credential path (~/.netrc) must still be present (resolved).
	wantNetrc := filepath.Join(realHome, ".netrc")
	if err := os.WriteFile(wantNetrc, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = buildMaskSet(linkHome, "", wt)
	found := false
	for _, m := range got {
		if m == wantNetrc {
			found = true
		}
	}
	if !found {
		t.Fatalf("sibling ~/.netrc should still be masked (resolved), got %v", got)
	}
}

// TestMaskSensitivePathsSkipsUnstatable covers FIX #18: a target whose stat fails
// with something other than ENOENT/ENOTDIR (here EACCES via a mode-000 parent) is
// skipped, not fatal — the command shares our uid, so an unstatable path is already
// unreadable to it. Fail-closed is reserved for mount failures on statable paths.
func TestMaskSensitivePathsSkipsUnstatable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not block stat, cannot induce EACCES")
	}
	base := t.TempDir()
	locked := filepath.Join(base, "locked") // mode 000: traversal into it is EACCES
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) // let TempDir cleanup remove it
	target := filepath.Join(locked, "config.json")    // stat(target) => EACCES

	// Sanity: confirm the stat really fails with a non-ENOENT/ENOTDIR error, else
	// the test is not exercising the skip branch.
	if _, err := os.Stat(target); err == nil {
		t.Skip("stat unexpectedly succeeded (test runs with elevated traversal rights)")
	}
	if err := maskSensitivePaths([]string{target}); err != nil {
		t.Fatalf("unstatable target must be skipped, not fatal: %v", err)
	}
}

// TestNamespaceMasksCredentialPaths is the end-to-end I3 proof: even though
// Landlock grants read on "/", a sandboxed command must not be able to read the
// operator's credential files. The operator home and config dir are faked via
// HOME / XDG_CONFIG_HOME — exactly the values the parent resolves in
// ExecWithEnv — populated with sentinel content, and the sandboxed probe must
// come back empty. It covers both mask mechanisms: the directory tmpfs
// (~/.ssh, the config dir) and the /dev/null file bind (~/.netrc).
func TestNamespaceMasksCredentialPaths(t *testing.T) {
	box := requireNamespace(t)

	const sentinel = "SENTINEL-CREDENTIAL-MATERIAL"
	fakeHome := t.TempDir()
	sshDir := filepath.Join(fakeHome, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(sshDir, "id_ed25519")
	if err := os.WriteFile(keyFile, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	netrc := filepath.Join(fakeHome, ".netrc")
	if err := os.WriteFile(netrc, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgBase := t.TempDir()
	cfgDir := filepath.Join(cfgBase, "nilcore")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	vault := filepath.Join(cfgDir, "secrets.vault")
	if err := os.WriteFile(vault, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	// The parent resolves the mask roots from its own env at ExecWithEnv time.
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_CONFIG_HOME", cfgBase)

	probe := fmt.Sprintf(
		`for f in %s %s %s; do cat "$f" 2>/dev/null; done; ls -A %s 2>/dev/null; echo probe-done`,
		keyFile, netrc, vault, sshDir)
	res := runSandbox(t, box, probe)

	if strings.Contains(res.Stdout, sentinel) {
		t.Fatalf("I3 LEAK: credential contents readable inside the sandbox:\n%s", res.Stdout)
	}
	if strings.Contains(res.Stdout, "id_ed25519") {
		t.Fatalf("masked ~/.ssh should list empty, got %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "probe-done") {
		t.Fatalf("probe did not complete: stdout %q stderr %q", res.Stdout, res.Stderr)
	}

	// The masks live only in the sandbox's mount namespace — the host view is
	// untouched (MS_PRIVATE keeps them from propagating back).
	for _, f := range []string{keyFile, netrc, vault} {
		if b, err := os.ReadFile(f); err != nil || string(b) != sentinel {
			t.Fatalf("host copy of %s must be untouched: err=%v content=%q", f, err, b)
		}
	}
}

// TestNamespaceMaskSparesWorktree proves the workdir-overlap exclusion end to
// end: when the worktree itself lives under a would-be mask target (here the
// fake home's .claude), that target is not masked and the worktree stays fully
// usable — while sibling credential paths are still masked.
func TestNamespaceMaskSparesWorktree(t *testing.T) {
	if ok, reason := detectNamespace(); !ok {
		if os.Getenv("NILCORE_SANDBOX_MUST_RUN") == "1" {
			t.Fatalf("namespace backend required (NILCORE_SANDBOX_MUST_RUN=1) but unavailable: %s", reason)
		}
		t.Skipf("namespace backend unavailable on this host: %s", reason)
	}

	const sentinel = "SENTINEL-CREDENTIAL-MATERIAL"
	fakeHome := t.TempDir()
	wt := filepath.Join(fakeHome, ".claude", "worktrees", "wt")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	netrc := filepath.Join(fakeHome, ".netrc")
	if err := os.WriteFile(netrc, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)

	box, err := newNamespace(wt)
	if err != nil {
		t.Fatalf("newNamespace: %v", err)
	}
	res := runSandbox(t, box.(*Namespace),
		fmt.Sprintf(`echo ok > inside.txt && cat inside.txt && cat %s 2>/dev/null; echo probe-done`, netrc))
	if !strings.Contains(res.Stdout, "ok") {
		t.Fatalf("worktree under a mask candidate must stay writable, got %q stderr %q", res.Stdout, res.Stderr)
	}
	if strings.Contains(res.Stdout, sentinel) {
		t.Fatalf("I3 LEAK: sibling credential file readable: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "probe-done") {
		t.Fatalf("probe did not complete: stdout %q stderr %q", res.Stdout, res.Stderr)
	}
}
