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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"nilcore/internal/blastbudget"
)

// defaultPerExecWall bounds a single exec when the caller's context carries no
// deadline. It only matters when a *blastbudget.Budget is attached (the field is
// nil by default), so an unwired sandbox is unaffected by this constant.
const defaultPerExecWall = 30 * time.Minute

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

	// ExtraHosts are `--add-host` entries (e.g. "host.docker.internal:host-gateway")
	// so a bridged container can resolve the host running the egress allowlist proxy.
	// Empty by default; set only when egress is enabled and the runtime needs it
	// (docker on Linux — podman and Docker Desktop provide the host alias already).
	ExtraHosts []string

	// DNS, when non-empty, is emitted as `--dns <DNS>` so the container resolves
	// names only through the given resolver. It is set by the HARD egress path
	// (AllowEgressViaHard): pointing the sandbox's resolver at the dual-homed gateway
	// (which serves no :53) blackholes in-sandbox DNS, closing the DNS-tunnel exfil
	// residual — proxied traffic still works because the client reaches the proxy by
	// IP and the proxy does the resolving. Empty by default (byte-identical run args).
	DNS string

	// ExtraReadRoots are additional host directories bind-mounted READ-ONLY into the
	// container at the SAME absolute path (identity-mapped, so a path the host-side
	// file tools already resolved is the same path the in-box `run` shell sees). They
	// back the user's `/add <path>` context roots so an execute-mode shell can read
	// them too. Each is mounted `:ro`, so /work stays the single WRITABLE mount (I4).
	// Empty by default — non-/add runs are byte-identical. Note: under rootless
	// podman (--userns=keep-id) a root owned by another user reads as empty inside the
	// box (a uid-mapping limitation, not an error); we do not chown.
	ExtraReadRoots []string

	// Blast is the optional blast-radius budget (Phase 16, BR-T03). When set, every
	// exec is fenced on the cumulative sandbox WALL-TIME axis: the per-exec bound is
	// pre-charged before the command runs (and the run is hard-bounded to that budget
	// via context.WithTimeout), then reconciled to the actual elapsed time after.
	// nil (the default) means no fence — behaviour and run args are byte-identical to
	// a sandbox without a budget.
	Blast *blastbudget.Budget

	// run is the exec seam, injected only in tests so the wall-time fence's
	// charge/reconcile path is exercisable without launching a real container. nil
	// (the default) uses runReal, so production behaviour is unchanged.
	run func(ctx context.Context, args []string) (Result, error)
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
// (--network none, the safe default). The proxy enforces the policy.Egress
// allowlist for proxy-respecting clients.
//
// SECURITY (important): this sets `--network bridge`, which gives the container a
// real NAT route to the internet, and points HTTP(S)_PROXY at the allowlist proxy.
// The allowlist is therefore COOPERATIVE, not a hard boundary — a model-emitted
// command that ignores the proxy (curl --noproxy, raw sockets, bash /dev/tcp) can
// still reach arbitrary hosts, including the cloud-metadata endpoint. For a HARD
// egress boundary use the namespace backend (Linux), which runs with an empty
// network namespace (deny-all). applyContainerEgress (cmd/nilcore) warns about this
// and honors NILCORE_EGRESS_STRICT to fail closed.
//
// The container backend has an OPT-IN hard option too: AllowEgressViaHard (wired by
// applyContainerEgress under NILCORE_EGRESS_HARD) attaches the sandbox to a
// `--internal` network with no route out and routes it through a dual-homed gateway
// container, making the allowlist unbypassable (Linux-container only, CI-validated,
// with an honestly-documented DNS-tunnel residual). The namespace backend's empty
// netns remains the recommended hard boundary.
//
// NOTE: allowlisted egress is a CONTAINER-backend capability only. The namespace
// backend (Auto-preferred on Linux) has no proxy path — it is hard deny-all (see
// namespace_linux.go). Callers that need web_fetch / a non-empty egress allowlist
// must run on the container backend (`-sandbox container`).
func (c *Container) AllowEgressVia(proxyURL string) {
	c.Network = "bridge"
	c.setProxyEnv(proxyURL)
}

// AllowEgressViaHard is the HARD egress path (opt-in, Linux-container only; wired by
// cmd/nilcore's applyContainerEgress under NILCORE_EGRESS_HARD). Unlike AllowEgressVia
// it does NOT attach the container to a bridged NAT network. Instead the caller has
// created a dedicated `--internal` docker/podman network — which has NO default route
// — and runs the allowlist proxy as a dual-homed GATEWAY container (internal net +
// a normal net). The sandbox is attached to the INTERNAL net only, so its ONLY path
// off-box is the gateway: proxy-cooperative traffic reaches the allowlist, while a
// raw socket / `curl --noproxy` has no route out and simply fails. This makes the
// allowlist UNBYPASSABLE without host root / NET_ADMIN (the sandbox keeps
// --cap-drop=ALL), i.e. a genuine boundary rather than the cooperative one.
//
// network is the internal network name (emitted as `--network`, so no bridge and no
// --add-host are needed); proxyURL points HTTP(S)_PROXY at the gateway. The caller
// additionally sets c.DNS to the gateway so in-sandbox DNS is blackholed (the
// remaining residual — see the DNS field). HONEST RESIDUALS (documented in
// applyContainerEgress + docs/ARCHITECTURE.md): DNS-tunnel exfil is only mitigated,
// not proven-closed; it requires the nilcore image (a debian:stable-slim has no
// `nilcore` to run the gateway); it is Linux-container only and CI-validated. The
// namespace backend's empty netns remains the recommended hard boundary.
func (c *Container) AllowEgressViaHard(network, proxyURL string) {
	c.Network = network
	c.setProxyEnv(proxyURL)
}

// setProxyEnv points the four HTTP(S)_PROXY vars at proxyURL and pins NO_PROXY empty
// so an inherited NO_PROXY can't exempt any host from the proxy (defense-in-depth;
// on the cooperative bridge path it does NOT stop a client that bypasses the proxy
// entirely — see the SECURITY note above — whereas on the hard path there is no
// route around the proxy at all).
func (c *Container) setProxyEnv(proxyURL string) {
	if c.Env == nil {
		c.Env = map[string]string{}
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		c.Env[k] = proxyURL
	}
	c.Env["NO_PROXY"] = ""
	c.Env["no_proxy"] = ""
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

	// Host aliases for a bridged container to reach the host (e.g. the egress proxy
	// on docker-Linux). Empty unless egress wiring set them.
	for _, h := range c.ExtraHosts {
		args = append(args, "--add-host", h)
	}

	// Pin the resolver to a single DNS server (the HARD egress path points this at
	// the gateway so in-sandbox DNS is blackholed). Empty unless hard egress set it.
	if c.DNS != "" {
		args = append(args, "--dns", c.DNS)
	}

	// Per-run secret injection (P2-T03): keys reach the container only for this
	// invocation, never persisted, never logged.
	for k, v := range c.Env {
		args = append(args, "-e", k+"="+v)
	}
	for k, v := range perRun {
		args = append(args, "-e", k+"="+v)
	}

	// Extra READ-ONLY context roots (the user's /add <path>), identity-mapped so the
	// in-box path equals the host path. Mounted before /work; each is :ro so the
	// worktree at /work stays the only writable mount (I4).
	for _, r := range c.ExtraReadRoots {
		args = append(args, "-v", fmt.Sprintf("%s:%s:ro", r, r))
	}

	args = append(args, "-v", fmt.Sprintf("%s:/work", c.HostDir), "-w", "/work", c.Image, "sh", "-c", cmd)
	return args
}

// Exec runs cmd with no extra per-run environment.
func (c *Container) Exec(ctx context.Context, cmd string) (Result, error) {
	return c.ExecWithEnv(ctx, cmd, nil)
}

// ExecWithEnv runs cmd, injecting env into the container for this invocation only.
//
// When a blast-radius budget is attached (c.Blast != nil), the run is fenced on
// the cumulative sandbox wall-time axis: a per-exec bound (the context's remaining
// deadline, or defaultPerExecWall) is pre-charged via ChargeWall BEFORE the
// command runs. If that charge is refused the real command NEVER runs and we
// return a non-zero Result (a budget-refused command is a result, not a Go error,
// matching the package's exit-code convention). The bound also hard-caps the
// in-flight run via context.WithTimeout, so the fence bounds wall-time rather than
// only accounting for it. After the run, accounting is reconciled to the actual
// elapsed time (charge the real elapsed, credit the unused remainder), keeping the
// cumulative wall total honest. With c.Blast == nil this whole block is skipped and
// the run is byte-identical to today.
func (c *Container) ExecWithEnv(ctx context.Context, cmd string, env map[string]string) (Result, error) {
	args := c.runArgs(cmd, env)

	if c.Blast != nil {
		bound := execWallBound(ctx)
		// Cap the pre-charge at the REMAINING wall budget: a single exec is then
		// hard-bounded to what's left (the adversarial-review fix — the fence
		// bounds, it does not merely account), and the pre-charge never spuriously
		// refuses while budget remains (without this, a no-deadline ctx would
		// pre-charge the full defaultPerExecWall and refuse the first exec under any
		// smaller ceiling). An already-exhausted budget refuses before the command runs.
		if u := c.Blast.Used(""); u.WallCeiling > 0 {
			rem := u.WallCeiling - u.Wall
			if rem <= 0 {
				return Result{Stderr: "blast-radius: sandbox wall-time budget exhausted", ExitCode: 1}, nil
			}
			if rem < bound {
				bound = rem
			}
		}
		if err := c.Blast.ChargeWall(ctx, bound); err != nil {
			if errors.Is(err, blastbudget.ErrWallCeiling) {
				return Result{
					Stderr:   "blast-radius: sandbox wall-time budget exhausted",
					ExitCode: 1,
				}, nil
			}
			return Result{}, fmt.Errorf("blast wall pre-charge: %w", err)
		}

		// The pre-charged bound also bounds the in-flight exec, so a runaway command
		// cannot outlive the wall budget it was charged for.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, bound)
		defer cancel()

		start := time.Now()
		res, err := c.runOnce(ctx, args)
		// Reconcile: keep only the actual elapsed on the wall axis and return the
		// unused remainder, so the cumulative total never over-counts a fast exec.
		actual := time.Since(start)
		if actual > bound {
			actual = bound
		}
		c.Blast.CreditWall(bound - actual)
		return res, err
	}

	return c.runOnce(ctx, args)
}

// runOnce dispatches to the injected exec seam (tests) or the real container exec.
func (c *Container) runOnce(ctx context.Context, args []string) (Result, error) {
	if c.run != nil {
		return c.run(ctx, args)
	}
	return c.runReal(ctx, args)
}

// runReal launches the container runtime with args. It is the production exec path;
// behaviour is identical to the pre-fence ExecWithEnv body.
func (c *Container) runReal(ctx context.Context, args []string) (Result, error) {
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

// execWallBound derives the wall-time bound to pre-charge for a single exec: the
// context's remaining deadline when one is set, otherwise a sensible per-exec cap.
func execWallBound(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining > 0 {
			return remaining
		}
		return 0
	}
	return defaultPerExecWall
}
