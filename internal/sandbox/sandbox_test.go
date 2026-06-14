package sandbox

import (
	"strings"
	"testing"
)

func argsString(c *Container, cmd string) string {
	return strings.Join(c.runArgs(cmd), " ")
}

func TestRunArgsHardenedDocker(t *testing.T) {
	c := NewContainer("docker", "img", "/work")
	got := argsString(c, "go test ./...")
	for _, want := range []string{
		"--network none",
		"--cap-drop=ALL",
		"--security-opt no-new-privileges",
		"--read-only",
		"--tmpfs /tmp",
		"GOCACHE=/tmp/.gocache",
		"--user",
		"/work:/work",
		"-w /work",
		"sh -c go test ./...",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %s", want, got)
		}
	}
}

func TestRunArgsPodmanUserns(t *testing.T) {
	c := NewContainer("podman", "img", "/work")
	got := argsString(c, "echo hi")
	if !strings.Contains(got, "--userns=keep-id") {
		t.Errorf("podman should map host uid via keep-id: %s", got)
	}
	if strings.Contains(got, "--user ") {
		t.Errorf("podman should not pass --user: %s", got)
	}
}

func TestHardeningDisabled(t *testing.T) {
	c := NewContainer("docker", "img", "/work")
	c.Hardened = false
	got := argsString(c, "x")
	for _, unwanted := range []string{"--cap-drop", "--read-only", "no-new-privileges"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("hardening disabled but found %q: %s", unwanted, got)
		}
	}
	// The mount and command must still be present.
	if !strings.Contains(got, "/work:/work") || !strings.Contains(got, "sh -c x") {
		t.Errorf("core run args missing: %s", got)
	}
}

func TestEnvInjection(t *testing.T) {
	c := NewContainer("docker", "img", "/work")
	c.Env = map[string]string{"ANTHROPIC_API_KEY": "sk-xyz"}
	got := argsString(c, "x")
	if !strings.Contains(got, "ANTHROPIC_API_KEY=sk-xyz") {
		t.Errorf("env not injected: %s", got)
	}
}
