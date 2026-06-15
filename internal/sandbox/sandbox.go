// Package sandbox runs commands inside an isolated boundary (invariant I4). It
// ships two backends behind the Sandbox interface: a container (docker/podman)
// and a host-native namespace backend (Linux user/mount/pid/net namespaces +
// Landlock) that needs no runtime, image, or daemon. New auto-detects and
// prefers the namespace backend wherever the kernel supports it, falling back to
// a container otherwise. Both run hardened: dropped privileges, a read-only view
// of everything outside the worktree, writable scratch, and default-deny egress.
// A microVM backend can satisfy the same interface later without touching any
// caller.
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
)

// Result is the outcome of one command. A non-zero ExitCode is a normal result,
// not a Go error — the agent is expected to react to failing commands.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Sandbox executes a shell command against a project directory. ExecWithEnv
// injects per-invocation environment (e.g. a delegated CLI's API key, P2-T03):
// the values reach the container only for that single run and are never logged.
type Sandbox interface {
	Exec(ctx context.Context, cmd string) (Result, error)
	ExecWithEnv(ctx context.Context, cmd string, env map[string]string) (Result, error)
	Workdir() string
}

// Container runs each command inside a throwaway container. The host worktree is
// bind-mounted read-write at /work; networking is denied by default (an egress
// allowlist is Phase-2 policy, P2-T02).
type Container struct {
	Runtime  string            // "podman" (preferred, rootless) or "docker"
	Image    string            // sandbox image
	HostDir  string            // absolute path to the worktree on the host
	Network  string            // "none" by default
	Hardened bool              // apply the hardening flags (default true)
	UID, GID int               // host uid/gid the container maps to
	Env      map[string]string // per-run env injected into the container (P2-T03)
}

// NewContainer returns a hardened container executor for the given worktree.
func NewContainer(runtime, image, hostDir string) *Container {
	if runtime == "" {
		runtime = "podman"
	}
	if image == "" {
		image = "docker.io/library/debian:stable-slim"
	}
	return &Container{
		Runtime:  runtime,
		Image:    image,
		HostDir:  hostDir,
		Network:  "none",
		Hardened: true,
		UID:      os.Getuid(),
		GID:      os.Getgid(),
	}
}

func (c *Container) Workdir() string { return c.HostDir }

// AllowEgressVia routes the container's network through an allowlist proxy
// (proxyURL, e.g. policy.ProxyURL(addr)). Without this, egress is denied entirely
// (--network none). The proxy enforces the policy.Egress allowlist, so only
// approved hosts are reachable even though the container now has a network.
func (c *Container) AllowEgressVia(proxyURL string) {
	c.Network = "bridge"
	if c.Env == nil {
		c.Env = map[string]string{}
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		c.Env[k] = proxyURL
	}
}

// runArgs builds the container runtime argument list (extracted so the hardening
// flags are unit-testable without launching a container). perRun env is merged
// on top of the container's persistent Env, for per-invocation secret injection.
func (c *Container) runArgs(cmd string, perRun map[string]string) []string {
	args := []string{"run", "--rm", "--network", c.Network}

	if c.Hardened {
		// Minimize blast radius: no capabilities, no privilege escalation, an
		// immutable rootfs with a writable tmpfs for scratch (Go caches live there
		// via the env below), and the worktree mapped to the host user so /work
		// stays writable without running as root.
		args = append(args,
			"--cap-drop=ALL",
			"--security-opt", "no-new-privileges",
			"--read-only",
			"--tmpfs", "/tmp",
			"-e", "HOME=/tmp",
			"-e", "GOCACHE=/tmp/.gocache",
			"-e", "GOPATH=/tmp/.gopath",
		)
		if c.Runtime == "podman" {
			args = append(args, "--userns=keep-id")
		} else if c.UID >= 0 {
			args = append(args, "--user", fmt.Sprintf("%d:%d", c.UID, c.GID))
		}
	}

	// Per-run secret injection (P2-T03): keys reach the container only for this
	// invocation, never persisted, never logged.
	for k, v := range c.Env {
		args = append(args, "-e", k+"="+v)
	}
	for k, v := range perRun {
		args = append(args, "-e", k+"="+v)
	}

	args = append(args, "-v", fmt.Sprintf("%s:/work", c.HostDir), "-w", "/work", c.Image, "sh", "-c", cmd)
	return args
}

// Exec runs cmd with no extra per-run environment.
func (c *Container) Exec(ctx context.Context, cmd string) (Result, error) {
	return c.ExecWithEnv(ctx, cmd, nil)
}

// ExecWithEnv runs cmd, injecting env into the container for this invocation only.
func (c *Container) ExecWithEnv(ctx context.Context, cmd string, env map[string]string) (Result, error) {
	var stdout, stderr bytes.Buffer
	ec := exec.CommandContext(ctx, c.Runtime, c.runArgs(cmd, env)...)
	ec.Stdout = &stdout
	ec.Stderr = &stderr
	err := ec.Run()

	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("%s run: %w", c.Runtime, err)
	}
	return res, nil
}
