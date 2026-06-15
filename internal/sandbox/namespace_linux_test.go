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
	res := runSandbox(t, box, "echo scratch > /tmp/nilcore-x && cat /tmp/nilcore-x")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "scratch") {
		t.Fatalf("/tmp should be writable scratch: exit %d out %q stderr %q", res.ExitCode, res.Stdout, res.Stderr)
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
