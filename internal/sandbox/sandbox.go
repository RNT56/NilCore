// Package sandbox runs commands inside an isolated boundary. Phase 0 ships the
// container backend (docker or podman); a microVM or namespace backend can
// satisfy the same interface later without touching any caller.
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Result is the outcome of one command. A non-zero ExitCode is a normal result,
// not a Go error — the agent is expected to react to failing commands.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Sandbox executes a shell command against a project directory.
type Sandbox interface {
	Exec(ctx context.Context, cmd string) (Result, error)
	Workdir() string
}

// Container runs each command inside a throwaway container. The host workdir
// (a git worktree, in normal use) is bind-mounted read-write at /work, and
// networking is disabled by default. An egress allowlist is a Phase 1 policy.
type Container struct {
	Runtime string // "podman" (preferred, rootless) or "docker"
	Image   string
	HostDir string // absolute path to the worktree on the host
	Network string // "none" by default
}

func NewContainer(runtime, image, hostDir string) *Container {
	if runtime == "" {
		runtime = "podman"
	}
	if image == "" {
		image = "docker.io/library/debian:stable-slim"
	}
	return &Container{Runtime: runtime, Image: image, HostDir: hostDir, Network: "none"}
}

func (c *Container) Workdir() string { return c.HostDir }

func (c *Container) Exec(ctx context.Context, cmd string) (Result, error) {
	args := []string{
		"run", "--rm",
		"--network", c.Network,
		"-v", fmt.Sprintf("%s:/work", c.HostDir),
		"-w", "/work",
		c.Image,
		"sh", "-c", cmd,
	}

	var stdout, stderr bytes.Buffer
	ec := exec.CommandContext(ctx, c.Runtime, args...)
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
