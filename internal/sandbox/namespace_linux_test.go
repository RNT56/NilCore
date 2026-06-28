//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as the sandbox init for the re-exec: when ExecWithEnv re-execs
// THIS test binary with the marker env, MaybeRunInit applies confinement and
// execve's the command (never returning), so the tests exercise the real init +
// Landlock path end to end. Without the marker it's a no-op and the suite runs.
func TestMain(m *testing.M) {
	MaybeRunInit()
	os.Exit(m.Run())
}

// requireNamespace returns a Namespace bound to a throwaway worktree, or skips —
// unless NILCORE_SANDBOX_MUST_RUN=1, where it fails instead. The dedicated CI job
// sets that var so the security tests can never silently skip into a false green.
func requireNamespace(t *testing.T) *Namespace {
	t.Helper()
	ok, reason := detectNamespace()
	if !ok {
		if os.Getenv("NILCORE_SANDBOX_MUST_RUN") == "1" {
			t.Fatalf("namespace backend required (NILCORE_SANDBOX_MUST_RUN=1) but unavailable: %s", reason)
		}
		t.Skipf("namespace backend unavailable on this host: %s", reason)
	}
	box, err := newNamespace(t.TempDir())
	if err != nil {
		t.Fatalf("newNamespace: %v", err)
	}
	return box.(*Namespace)
}

func runSandbox(t *testing.T, box *Namespace, cmd string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := box.ExecWithEnv(ctx, cmd, nil)
	if err != nil {
		t.Fatalf("exec %q: %v (stderr: %s)", cmd, err, res.Stderr)
	}
	return res
}

func TestNamespaceRunsCommand(t *testing.T) {
	box := requireNamespace(t)
	res := runSandbox(t, box, "echo hello-sandbox")
	if res.ExitCode != 0 {
		t.Fatalf("exit %d, stderr %q", res.ExitCode, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "hello-sandbox" {
		t.Fatalf("stdout = %q, want %q", res.Stdout, "hello-sandbox")
	}
}

func TestNamespaceWritesInsideWorktree(t *testing.T) {
	box := requireNamespace(t)
	res := runSandbox(t, box, "echo data > inside.txt && cat inside.txt")
	if res.ExitCode != 0 {
		t.Fatalf("write inside the worktree should succeed: exit %d stderr %q", res.ExitCode, res.Stderr)
	}
	if got, err := os.ReadFile(filepath.Join(box.HostDir, "inside.txt")); err != nil {
		t.Fatalf("file should exist on the host worktree: %v", err)
	} else if strings.TrimSpace(string(got)) != "data" {
		t.Fatalf("worktree file contents = %q", got)
	}
}

// TestNamespaceDeniesWriteOutsideWorktree is the core confinement guarantee: the
// host filesystem is read-only outside the worktree, enforced by Landlock.
func TestNamespaceDeniesWriteOutsideWorktree(t *testing.T) {
	box := requireNamespace(t)
	const escape = "/etc/nilcore-escape-probe"
	res := runSandbox(t, box, "echo escaped > "+escape+" 2>/dev/null; echo finished")

	if _, err := os.Stat(escape); err == nil {
		_ = os.Remove(escape)
		t.Fatalf("LANDLOCK BREACH: %s was created on the host", escape)
	}
	if strings.Contains(res.Stdout, "escaped") {
		t.Fatalf("the denied write should produce no output, got %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "finished") {
		t.Fatalf("the shell should continue past the denied write, got %q", res.Stdout)
	}
}

// TestNamespaceCannotReadHostSecrets proves read confinement is intentional: the
// host toolchain stays readable (we grant read+exec on "/"), but that is a
// deliberate choice, so this test only asserts the writable boundary holds for a
// nested path the worktree does not cover.
func TestNamespaceDeniesWriteToHome(t *testing.T) {
	box := requireNamespace(t)
	res := runSandbox(t, box, "echo x > /root/nilcore-escape 2>/dev/null; echo done")
	if _, err := os.Stat("/root/nilcore-escape"); err == nil {
		_ = os.Remove("/root/nilcore-escape")
		t.Fatal("LANDLOCK BREACH: wrote to /root from the sandbox")
	}
	if !strings.Contains(res.Stdout, "done") {
		t.Fatalf("unexpected output %q", res.Stdout)
	}
}

func TestNamespaceDevNullWritable(t *testing.T) {
	box := requireNamespace(t)
	res := runSandbox(t, box, "echo noise > /dev/null && echo ok")
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "ok" {
		t.Fatalf("/dev/null redirect should succeed: exit %d out %q stderr %q", res.ExitCode, res.Stdout, res.Stderr)
	}
}

func TestNamespaceTmpWritable(t *testing.T) {
	box := requireNamespace(t)
	// The writable scratch is $TMPDIR (== HOME), set to a run-private dir. A tool that
	// needs scratch writes there; this holds whether the worktree is under /tmp (a
	// run-private subdir) or not (a private tmpfs at /tmp). We no longer grant write to
	// all of shared host /tmp (the B3 isolation fix), so a tool must use $TMPDIR — which
	// the namespace backend sets for exactly this reason.
	res := runSandbox(t, box, `echo scratch > "$TMPDIR/nilcore-x" && cat "$TMPDIR/nilcore-x"`)
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "scratch") {
		t.Fatalf("$TMPDIR should be writable scratch: exit %d out %q stderr %q", res.ExitCode, res.Stdout, res.Stderr)
	}
}

// TestNamespaceDoesNotShareHostTmp proves the B3 isolation: when the worktree lives
// under /tmp (the production layout — worktrees are os.MkdirTemp("")), the sandbox does
// NOT get write access to all of shared host /tmp, only its run-private scratch
// ($TMPDIR). So a concurrent run cannot scribble into another run's /tmp space. A write
// to a /tmp path OUTSIDE the scratch must be denied. The workdir is rooted explicitly
// under /tmp so this holds regardless of the host's $TMPDIR.
func TestNamespaceDoesNotShareHostTmp(t *testing.T) {
	if ok, reason := detectNamespace(); !ok {
		if os.Getenv("NILCORE_SANDBOX_MUST_RUN") == "1" {
			t.Fatalf("namespace backend required (NILCORE_SANDBOX_MUST_RUN=1) but unavailable: %s", reason)
		}
		t.Skipf("namespace backend unavailable on this host: %s", reason)
	}
	dir, err := os.MkdirTemp("/tmp", "nilcore-b3-")
	if err != nil {
		t.Skipf("cannot create a /tmp-rooted workdir: %v", err)
	}
	defer os.RemoveAll(dir)
	box, err := newNamespace(dir)
	if err != nil {
		t.Fatalf("newNamespace: %v", err)
	}
	res := runSandbox(t, box.(*Namespace),
		`if echo x > /tmp/nilcore-shared-probe 2>/dev/null; then echo WROTE; else echo denied; fi`)
	if !strings.Contains(res.Stdout, "denied") {
		t.Fatalf("a write to shared host /tmp must be DENIED (B3 isolation), got out=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

// TestNamespaceDeniesEgress checks the network namespace has no route out. bash's
// /dev/tcp is a dependency-free connect attempt; with no configured interface the
// connect fails fast (ENETUNREACH) and must never report success. If bash is
// absent the probe simply can't run — it never false-fails.
func TestNamespaceDeniesEgress(t *testing.T) {
	box := requireNamespace(t)
	res := runSandbox(t, box,
		`command -v bash >/dev/null 2>&1 && timeout 5 bash -c 'exec 3<>/dev/tcp/1.1.1.1/80 && echo CONNECTED' 2>/dev/null; echo probe-done`)
	if strings.Contains(res.Stdout, "CONNECTED") {
		t.Fatalf("NETWORK BREACH: egress succeeded from the network namespace:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "probe-done") {
		t.Fatalf("unexpected output %q", res.Stdout)
	}
}

// TestNamespaceForwardsPerRunEnv confirms per-run env reaches the command (the
// secret-injection contract) and that the control vars are stripped.
func TestNamespaceForwardsPerRunEnv(t *testing.T) {
	box := requireNamespace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := box.ExecWithEnv(ctx, `printf '%s\n' "$NILCORE_TEST_TOKEN"; printf 'marker=%s\n' "$NILCORE_SANDBOX_INIT"`,
		map[string]string{"NILCORE_TEST_TOKEN": "s3cr3t"})
	if err != nil {
		t.Fatalf("exec: %v (stderr %s)", err, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "s3cr3t") {
		t.Fatalf("per-run env should reach the command, got %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "marker=\n") && !strings.HasSuffix(strings.TrimRight(res.Stdout, "\n"), "marker=") {
		t.Fatalf("the sandbox control var should be stripped from the command env, got %q", res.Stdout)
	}
}

// TestNamespaceDoesNotLeakHostEnv is the I3 regression: a secret that lives in
// the HOST process environment (as ANTHROPIC_API_KEY does in an env-based
// deployment) must never reach a model-emitted command — the namespace backend
// does not inherit os.Environ(). It also confirms the fixed base env (PATH) is
// still provided so ordinary commands work.
func TestNamespaceDoesNotLeakHostEnv(t *testing.T) {
	box := requireNamespace(t)
	const hostKey, hostVal = "NILCORE_FAKE_HOST_SECRET", "leaked-api-key-value"
	t.Setenv(hostKey, hostVal) // restored automatically after the test

	res := runSandbox(t, box, `printf 'secret=[%s] path=[%s]' "$NILCORE_FAKE_HOST_SECRET" "${PATH:+set}"`)
	if strings.Contains(res.Stdout, hostVal) {
		t.Fatalf("I3 LEAK: host secret reached the sandboxed command: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "secret=[]") {
		t.Fatalf("host env var should be empty in the command, got %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "path=[set]") {
		t.Fatalf("the command should still get a base PATH, got %q", res.Stdout)
	}
}
