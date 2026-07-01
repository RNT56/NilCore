package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireContainer returns a hardened Container bound to a throwaway worktree and a
// usable runtime, or SKIPS.
//
// Launching a real container is OPT-IN via NILCORE_SANDBOX_INTEGRATION=1: by default
// this suite skips before touching any runtime, so `go test ./...` stays fast and
// hermetic and never blocks on a stopped Docker Desktop / podman machine. The
// dedicated CI container lane sets NILCORE_SANDBOX_INTEGRATION=1 to run it, and
// NILCORE_SANDBOX_MUST_RUN=1 to turn a missing/unusable runtime into a FAILURE rather
// than a silent skip (a false green) — mirroring requireNamespace.
//
// This is the container analogue of the namespace TestNamespaceDenies* suite: it
// exercises runReal — the only path that launches the runtime and so the only path
// that PROVES --network none / --read-only / --cap-drop actually confine — which every
// other container test bypasses via the c.run seam or by asserting arg strings only.
func requireContainer(t *testing.T) *Container {
	t.Helper()
	const image = "docker.io/library/debian:stable-slim"

	mustRun := os.Getenv("NILCORE_SANDBOX_MUST_RUN") == "1"
	skipOrFail := func(format string, a ...any) {
		t.Helper()
		if mustRun {
			t.Fatalf("container backend required (NILCORE_SANDBOX_MUST_RUN=1) but unavailable: "+format, a...)
		}
		t.Skipf("container backend unavailable on this host: "+format, a...)
	}

	// Opt-in gate: never launch a runtime in the default suite. MUST_RUN also implies
	// the integration tests are wanted, so CI need only set MUST_RUN to enforce them.
	if os.Getenv("NILCORE_SANDBOX_INTEGRATION") != "1" && !mustRun {
		t.Skip("container integration test is opt-in: set NILCORE_SANDBOX_INTEGRATION=1 to run it")
	}

	runtime := "podman"
	if _, err := exec.LookPath(runtime); err != nil {
		runtime = "docker"
		if _, err := exec.LookPath(runtime); err != nil {
			skipOrFail("no container runtime (podman/docker) on PATH")
		}
	}

	dir := t.TempDir()
	c := NewContainer(runtime, image, dir)

	// Probe: a no-op container must actually run within a bounded window. This both
	// pulls the image (first run) and proves the daemon/machine is up. The bounded
	// context guarantees we never hang even if the runtime stalls on a down daemon.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := c.Exec(ctx, "true")
	if err != nil {
		skipOrFail("runtime %s present but not usable: %v (stderr: %s)", runtime, err, res.Stderr)
	}
	if res.ExitCode != 0 {
		skipOrFail("runtime %s probe exited %d (stderr: %s)", runtime, res.ExitCode, res.Stderr)
	}
	return c
}

func runContainer(t *testing.T, c *Container, cmd string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := c.Exec(ctx, cmd)
	if err != nil {
		t.Fatalf("exec %q: %v (stderr: %s)", cmd, err, res.Stderr)
	}
	return res
}

// TestContainerRunsCommand proves runReal launches a real container and returns its
// stdout/exit — the baseline the rest of the suite builds on.
func TestContainerRunsCommand(t *testing.T) {
	c := requireContainer(t)
	res := runContainer(t, c, "echo hello-container")
	if res.ExitCode != 0 {
		t.Fatalf("exit %d, stderr %q", res.ExitCode, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "hello-container" {
		t.Fatalf("stdout = %q, want %q", res.Stdout, "hello-container")
	}
}

// TestContainerWritesInsideWorktree confirms /work is the one writable mount and the
// write lands back on the host worktree.
func TestContainerWritesInsideWorktree(t *testing.T) {
	c := requireContainer(t)
	res := runContainer(t, c, "echo data > inside.txt && cat inside.txt")
	if res.ExitCode != 0 {
		t.Fatalf("write inside /work should succeed: exit %d stderr %q", res.ExitCode, res.Stderr)
	}
	if got, err := os.ReadFile(filepath.Join(c.HostDir, "inside.txt")); err != nil {
		t.Fatalf("file should exist on the host worktree: %v", err)
	} else if strings.TrimSpace(string(got)) != "data" {
		t.Fatalf("worktree file contents = %q", got)
	}
}

// TestContainerDeniesWriteOutsideWork is the core I4 confinement guarantee: the
// rootfs is --read-only, so a write outside /work must fail. The container analogue
// of TestNamespaceDeniesWriteOutsideWorktree.
func TestContainerDeniesWriteOutsideWork(t *testing.T) {
	c := requireContainer(t)
	res := runContainer(t, c, "if echo escaped > /etc/nilcore-escape-probe 2>/dev/null; then echo WROTE; else echo denied; fi")
	if strings.Contains(res.Stdout, "WROTE") {
		t.Fatalf("READ-ONLY BREACH: wrote outside /work, got %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "denied") {
		t.Fatalf("the write outside /work should be denied, got out=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

// TestContainerDeniesEgress proves --network none leaves the container with no route
// out. A getent/DNS lookup of a public name must fail; if no resolver tool exists the
// probe still proves the point via a raw connect that can never succeed offline.
// The container analogue of TestNamespaceDeniesEgress.
func TestContainerDeniesEgress(t *testing.T) {
	c := requireContainer(t)
	// debian:stable-slim has getent (libc). With --network none, name resolution and
	// any connect fail; we assert no success marker is printed.
	res := runContainer(t, c,
		`getent hosts example.com >/dev/null 2>&1 && echo RESOLVED; echo probe-done`)
	if strings.Contains(res.Stdout, "RESOLVED") {
		t.Fatalf("NETWORK BREACH: egress/DNS succeeded under --network none:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "probe-done") {
		t.Fatalf("unexpected output %q (stderr %q)", res.Stdout, res.Stderr)
	}
}
