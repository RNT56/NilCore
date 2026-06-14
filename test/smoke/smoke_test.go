// Package smoke is the end-to-end check that the native loop actually converges:
// given a repo whose test fails, the agent edits it inside the sandbox until the
// verifier (the project's own `go test`) passes. It is gated behind
// ANTHROPIC_API_KEY, a container runtime, and the sandbox image, and skips
// cleanly when any is absent — so `make verify` stays green without secrets.
//
// Manual run (with a key and the image built — see images/sandbox/README.md):
//
//	export ANTHROPIC_API_KEY=sk-...
//	podman build -t nilcore/sandbox:latest images/sandbox
//	go test ./test/smoke/ -run TestNativeLoopConverges -v
package smoke

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sandboxImage = "nilcore/sandbox:latest"

func TestNativeLoopConverges(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping end-to-end smoke test")
	}
	rt := detectRuntime()
	if rt == "" {
		t.Skip("no container runtime (podman/docker) found; skipping smoke test")
	}
	if !imagePresent(rt) {
		t.Skipf("sandbox image %q not found; build it with: %s build -t %s images/sandbox",
			sandboxImage, rt, sandboxImage)
	}

	root := repoRoot(t)

	// Build the nilcore binary from the current tree.
	bin := filepath.Join(t.TempDir(), "nilcore")
	build := exec.Command("go", "build", "-o", bin, "./cmd/nilcore")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build nilcore: %v\n%s", err, out)
	}

	// Copy the failing fixture into a throwaway worktree the sandbox can write.
	work := filepath.Join(t.TempDir(), "work")
	if err := copyTree(filepath.Join(root, "test", "fixtures", "failing-go"), work); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin,
		"-dir", work,
		"-goal", "The test in mathx_test.go fails. Fix the bug in the package so that `go test ./...` passes. Make the smallest change.",
		"-verify", "go test ./...",
		"-runtime", rt,
		"-image", sandboxImage,
		"-backend", "native",
		"-max-steps", "30",
		"-log", filepath.Join(t.TempDir(), "events.jsonl"),
	)
	out, err := cmd.CombinedOutput()
	t.Logf("nilcore output:\n%s", out)
	if err != nil {
		t.Fatalf("nilcore run failed: %v", err)
	}
	if !strings.Contains(string(out), "verified: true") {
		t.Fatalf("expected the verifier to pass; got:\n%s", out)
	}
}

// detectRuntime prefers podman (rootless), then docker.
func detectRuntime() string {
	for _, rt := range []string{"podman", "docker"} {
		if _, err := exec.LookPath(rt); err == nil {
			return rt
		}
	}
	return ""
}

func imagePresent(rt string) bool {
	return exec.Command(rt, "image", "inspect", sandboxImage).Run() == nil
}

// repoRoot returns the module root (two levels up from this package).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

// copyTree copies src into dst and makes everything world-writable so a non-root
// sandbox user can edit the worktree (until P2-T01 wires host-UID mapping).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o777)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
