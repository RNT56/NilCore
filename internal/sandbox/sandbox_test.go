package sandbox

import (
	"strings"
	"testing"
)

func argsString(c *Container, cmd string) string {
	return strings.Join(c.runArgs(cmd, nil), " ")
}

func TestExecWithEnvArgs(t *testing.T) {
	c := NewContainer("docker", "img", "/work")
	args := strings.Join(c.runArgs("codex exec", map[string]string{"CODEX_API_KEY": "sk-run"}), " ")
	if !strings.Contains(args, "CODEX_API_KEY=sk-run") {
		t.Errorf("per-run env not injected: %s", args)
	}
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

func TestEgressDefaultDeny(t *testing.T) {
	c := NewContainer("docker", "img", "/work")
	got := argsString(c, "x")
	if !strings.Contains(got, "--network none") {
		t.Errorf("default should deny egress (--network none): %s", got)
	}
	if strings.Contains(got, "HTTP_PROXY") {
		t.Errorf("default should not set a proxy: %s", got)
	}
}

func TestAllowEgressVia(t *testing.T) {
	c := NewContainer("docker", "img", "/work")
	c.AllowEgressVia("http://127.0.0.1:8888")
	got := argsString(c, "x")
	if !strings.Contains(got, "--network bridge") {
		t.Errorf("proxied egress should use a bridge network: %s", got)
	}
	if !strings.Contains(got, "HTTP_PROXY=http://127.0.0.1:8888") {
		t.Errorf("proxy env not set: %s", got)
	}
}

func TestAllowEgressViaHard(t *testing.T) {
	const net = "nilcore-egr-abc123"
	c := NewContainer("podman", "img", "/work")
	c.AllowEgressViaHard(net, "http://10.88.0.2:3128")
	c.DNS = "10.88.0.2"
	got := argsString(c, "x")

	// The internal net is emitted verbatim — NOT bridge.
	if !strings.Contains(got, "--network "+net) {
		t.Errorf("hard egress should attach the internal net: %s", got)
	}
	if strings.Contains(got, "--network bridge") {
		t.Errorf("hard egress must NOT use a bridge network: %s", got)
	}
	// Proxy env points at the gateway; NO_PROXY is pinned empty.
	if c.Env["HTTP_PROXY"] != "http://10.88.0.2:3128" || c.Env["HTTPS_PROXY"] != "http://10.88.0.2:3128" {
		t.Errorf("hard egress proxy env not set: %v", c.Env)
	}
	if v, ok := c.Env["NO_PROXY"]; !ok || v != "" {
		t.Errorf("hard egress must pin NO_PROXY empty, got %q (present=%v)", v, ok)
	}
	// The gateway resolver is pinned via --dns; no --add-host on the hard path.
	if !strings.Contains(got, "--dns 10.88.0.2") {
		t.Errorf("hard egress should pin --dns at the gateway: %s", got)
	}
	if strings.Contains(got, "--add-host") {
		t.Errorf("hard egress must add no --add-host: %s", got)
	}

	// No DNS set ⇒ no --dns emitted (byte-identical default).
	c2 := NewContainer("podman", "img", "/work")
	if strings.Contains(argsString(c2, "x"), "--dns") {
		t.Errorf("--dns present with no DNS set")
	}
}

func TestExtraReadRootsMountedReadOnly(t *testing.T) {
	c := NewContainer("podman", "img", "/work/tree")
	c.ExtraReadRoots = []string{"/host/lib", "/host/docs"}
	got := argsString(c, "x")
	for _, r := range []string{"/host/lib", "/host/docs"} {
		if !strings.Contains(got, "-v "+r+":"+r+":ro") {
			t.Errorf("extra read root %q not mounted identity + :ro: %s", r, got)
		}
	}
	// The worktree is still the only WRITABLE mount, and the extra mounts precede it.
	if !strings.Contains(got, "/work/tree:/work") {
		t.Errorf("worktree mount missing: %s", got)
	}
	if i, j := strings.Index(got, "/host/lib:/host/lib:ro"), strings.Index(got, "/work/tree:/work"); i < 0 || j < 0 || i > j {
		t.Errorf("extra read roots must be mounted before /work: %s", got)
	}
	// No ExtraReadRoots ⇒ no extra -v (byte-identical default).
	c2 := NewContainer("podman", "img", "/work/tree")
	if n := strings.Count(argsString(c2, "x"), " -v "); n != 1 {
		t.Errorf("default should have exactly one -v (the worktree), got %d", n)
	}
}

func TestExtraHostsAddHost(t *testing.T) {
	c := NewContainer("docker", "img", "/work")
	c.ExtraHosts = []string{"host.docker.internal:host-gateway"}
	got := argsString(c, "x")
	if !strings.Contains(got, "--add-host host.docker.internal:host-gateway") {
		t.Errorf("--add-host not wired for ExtraHosts: %s", got)
	}
	// No ExtraHosts ⇒ no --add-host (byte-identical default).
	c2 := NewContainer("podman", "img", "/work")
	if strings.Contains(argsString(c2, "x"), "--add-host") {
		t.Errorf("--add-host present with no ExtraHosts set")
	}
}
