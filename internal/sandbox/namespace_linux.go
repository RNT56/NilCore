//go:build linux

// The namespace backend confines a model-emitted command (invariant I4) with
// Linux kernel primitives instead of a container runtime: it is born inside
// fresh user/mount/pid/net/ipc/uts namespaces, then — in the re-exec'd child,
// before the command runs — masks the operator's credential paths (~/.ssh, the
// NilCore config dir holding the encrypted vault + its key, …) with empty
// read-only mounts (I3; see buildMaskSet), sets no_new_privs, a Landlock
// ruleset that maps
// read-only-everywhere + read-write-under-the-worktree (mirroring the container
// backend's read-only rootfs + writable /work + tmpfs + no-egress), and a
// seccomp-bpf syscall denylist (seccomp_linux.go — defense-in-depth). No daemon,
// no image, no root: it runs wherever the kernel has Landlock (5.13+) and
// unprivileged user namespaces, which is the whole portability win.
//
// The mechanism is a re-exec: ExecWithEnv runs THIS binary again with a marker
// env var and the namespace clone flags; MaybeRunInit (called first in main)
// detects the marker in the child, applies confinement on a locked OS thread,
// and execve's /bin/sh -c <cmd> — execve preserves the Landlock domain and
// no_new_privs into the command. The model's command only ever runs AFTER
// confinement is in place; our own pre-exec code is trusted harness code.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"nilcore/internal/blastbudget"
)

// Control vars the parent sets on the re-exec'd child. They tell MaybeRunInit
// what to confine and run; they are stripped from the command's own environment.
const (
	envMarker  = "NILCORE_SANDBOX_INIT"
	envWorkdir = "NILCORE_SANDBOX_WORKDIR"
	envCmd     = "NILCORE_SANDBOX_CMD"
	// Host-resolved roots for credential masking (buildMaskSet). The child's
	// deliberately minimal env carries no HOME/XDG_CONFIG_HOME, so it cannot
	// resolve these itself — the parent resolves them where the real environment
	// lives and hands them down. They are path NAMES, not secrets (I3 holds).
	envMaskHome = "NILCORE_SANDBOX_MASK_HOME"
	envMaskCfg  = "NILCORE_SANDBOX_MASK_CFG"
)

// Namespace runs each command in a throwaway set of Linux namespaces with a
// Landlock filesystem domain. It needs no container runtime, image, or daemon.
type Namespace struct {
	HostDir string // absolute, symlink-resolved path to the worktree

	// Blast is the optional blast-radius budget (Phase 16, BR-T03), identical in
	// meaning to *Container.Blast: when set, every exec is fenced on the cumulative
	// sandbox WALL-TIME axis (pre-charge → context.WithTimeout bound → reconcile).
	// This backend is Auto-preferred on Linux (Landlock + userns), so without it the
	// -blast-radius wall ceiling would go silently unenforced on the DEFAULT backend.
	// nil (the default) means no fence — behaviour and run args are byte-identical to
	// a namespace sandbox without a budget.
	Blast *blastbudget.Budget

	// run is the exec seam, injected only in tests so the wall-time fence's
	// charge/reconcile path is exercisable without a real re-exec. nil (the default)
	// uses runReal, so production behaviour is unchanged.
	run func(ctx context.Context, cmd string, env map[string]string) (Result, error)
}

func newNamespace(hostDir string) (Sandbox, error) {
	abs := hostDir
	if r, err := filepath.EvalSymlinks(hostDir); err == nil {
		abs = r
	}
	return &Namespace{HostDir: abs}, nil
}

func (n *Namespace) Workdir() string { return n.HostDir }

// Exec runs cmd with no extra per-run environment.
func (n *Namespace) Exec(ctx context.Context, cmd string) (Result, error) {
	return n.ExecWithEnv(ctx, cmd, nil)
}

// ExecWithEnv re-execs this binary inside fresh namespaces and runs cmd under
// Landlock. Per-run env (e.g. a delegated CLI's key, P2-T03) is forwarded to the
// command only, never logged — matching the container backend's contract.
//
// When a blast-radius budget is attached (n.Blast != nil), the run is fenced on
// the cumulative sandbox wall-time axis exactly like *Container.ExecWithEnv: a
// per-exec bound (the context's remaining deadline, or defaultPerExecWall, capped
// at the remaining wall budget) is pre-charged BEFORE the command runs. If that
// charge is refused the real command NEVER runs and we return a non-zero Result (a
// budget-refused command is a result, not a Go error). The bound also hard-caps the
// in-flight run via context.WithTimeout. After the run, accounting is reconciled to
// the actual elapsed time. With n.Blast == nil this whole block is skipped and the
// run is byte-identical to a namespace sandbox without a budget.
func (n *Namespace) ExecWithEnv(ctx context.Context, cmd string, env map[string]string) (Result, error) {
	if n.Blast == nil {
		return n.runOnce(ctx, cmd, env)
	}

	bound := execWallBound(ctx)
	// Cap the pre-charge at the REMAINING wall budget so a single exec is
	// hard-bounded to what's left, and a no-deadline ctx never spuriously refuses
	// the first exec under a smaller ceiling. An already-exhausted budget refuses
	// before the command runs. (Mirrors Container.ExecWithEnv.)
	if u := n.Blast.Used(""); u.WallCeiling > 0 {
		rem := u.WallCeiling - u.Wall
		if rem <= 0 {
			return Result{Stderr: "blast-radius: sandbox wall-time budget exhausted", ExitCode: 1}, nil
		}
		if rem < bound {
			bound = rem
		}
	}
	if err := n.Blast.ChargeWall(ctx, bound); err != nil {
		if errors.Is(err, blastbudget.ErrWallCeiling) {
			return Result{Stderr: "blast-radius: sandbox wall-time budget exhausted", ExitCode: 1}, nil
		}
		return Result{}, fmt.Errorf("blast wall pre-charge: %w", err)
	}

	fenceCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()

	start := time.Now()
	res, err := n.runOnce(fenceCtx, cmd, env)
	// Reconcile: keep only the actual elapsed on the wall axis, credit the unused
	// remainder, so the cumulative total never over-counts a fast exec.
	actual := time.Since(start)
	if actual > bound {
		actual = bound
	}
	n.Blast.CreditWall(bound - actual)
	return res, err
}

// runOnce dispatches to the injected exec seam (tests) or the real namespace exec.
func (n *Namespace) runOnce(ctx context.Context, cmd string, env map[string]string) (Result, error) {
	if n.run != nil {
		return n.run(ctx, cmd, env)
	}
	return n.runReal(ctx, cmd, env)
}

// runReal re-execs this binary inside fresh namespaces and runs cmd under Landlock.
// It is the production exec path; behaviour is identical to the pre-fence body.
func (n *Namespace) runReal(ctx context.Context, cmd string, env map[string]string) (Result, error) {
	self, err := os.Executable()
	if err != nil {
		return Result{}, fmt.Errorf("locate self for sandbox re-exec: %w", err)
	}

	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, self)
	c.Stdout = &stdout
	c.Stderr = &stderr
	// SECURITY (I3): do NOT seed the child from os.Environ(). The host process
	// holds the operator's secrets (ANTHROPIC_API_KEY, the vault passphrase, the
	// log-HMAC key) for its whole lifetime, and a model-emitted command must never
	// see them. The container backend is safe for the same reason — it never
	// forwards os.Environ() into the container. So the child gets ONLY our control
	// vars plus the explicitly-injected per-run env (e.g. a delegated CLI's key,
	// which IS meant for the command); sandboxEnv then assembles the command's real
	// env from that minimal set + a fixed base (PATH/HOME/Go caches).
	c.Env = []string{
		envMarker + "=1",
		envWorkdir + "=" + n.HostDir,
		envCmd + "=" + cmd,
	}
	// SECURITY (I3): resolve the operator's home and the NilCore config dir here,
	// in the parent, so sandboxInit can mask the credential paths beneath them
	// (~/.ssh, secrets.vault + secrets.key, …) before the command runs. An
	// unresolvable root simply masks nothing under it — the fail-closed contract
	// applies to MOUNT failures on paths that provably stat (see
	// maskSensitivePaths), not to a host with no resolvable HOME nor to a
	// credential path we cannot even stat (already unreadable to the command).
	if home, herr := os.UserHomeDir(); herr == nil && home != "" {
		c.Env = append(c.Env, envMaskHome+"="+home)
	}
	if cfg, cerr := nilcoreConfigDir(); cerr == nil && cfg != "" {
		c.Env = append(c.Env, envMaskCfg+"="+cfg)
	}
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	// The child is BORN in these namespaces (the kernel creates them at clone);
	// its fresh runtime then reaches MaybeRunInit. Mapping our uid/gid to 0 makes
	// the child root *inside the user namespace only* — it holds no capability
	// over any host-owned resource. Setgroups is denied (required for an
	// unprivileged gid mapping). CLONE_NEWNET with no interfaces is default-deny
	// egress, the equivalent of the container's --network none.
	//
	// LIMITATION (fail-closed by design): this backend supports ONLY deny-all
	// egress. It has no allowlist path — there is no Namespace analogue of the
	// container's AllowEgressVia (that would need a userspace network, e.g.
	// slirp4netns/pasta, routing through the policy egress proxy inside the netns).
	// Because Auto prefers this backend wherever Landlock + userns exist (see
	// select.go), the common Linux deployment cannot do allowlisted egress / web_fetch
	// unless the operator forces `-sandbox container`. This is a usability limitation,
	// not a sandbox-escape risk: egress simply fails closed (the safe direction).
	c.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNET | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}

	err = c.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("namespace sandbox run: %w", err)
	}
	return res, nil
}

// MaybeRunInit is the sandbox init half of the re-exec. cmd/nilcore calls it as
// the very first thing in main: in a normal invocation the marker is unset and
// this returns immediately; in the re-exec'd child it applies confinement and
// execve's the command, never returning. Fail-closed: any confinement error
// exits non-zero rather than running the command unconfined.
func MaybeRunInit() {
	if os.Getenv(envMarker) != "1" {
		return
	}
	// Pin to one OS thread: Landlock and no_new_privs apply to the calling thread
	// and are carried across the upcoming execve.
	runtime.LockOSThread()

	if err := sandboxInit(os.Getenv(envWorkdir), os.Getenv(envMaskHome), os.Getenv(envMaskCfg)); err != nil {
		fmt.Fprintf(os.Stderr, "nilcore sandbox init: %v\n", err)
		os.Exit(126)
	}
	argv := []string{"/bin/sh", "-c", os.Getenv(envCmd)}
	if err := unix.Exec("/bin/sh", argv, sandboxEnv(sandboxScratch(os.Getenv(envWorkdir)))); err != nil {
		fmt.Fprintf(os.Stderr, "nilcore sandbox exec: %v\n", err)
		os.Exit(127)
	}
}

// sandboxInit establishes the confinement. The security boundaries (credential
// masks, no_new_privs, Landlock) are fail-closed; cosmetic steps (private
// mounts, a fresh /proc) are best-effort — Landlock, not /proc, is the boundary.
// maskHome/maskCfg are the parent-resolved operator home and NilCore config dir
// (empty when the parent could not resolve them).
func sandboxInit(workdir, maskHome, maskCfg string) error {
	if workdir == "" {
		return errors.New("empty workdir")
	}

	// Don't let our mounts propagate back to the host mount namespace.
	_ = unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
	// A /proc reflecting our PID namespace (best-effort; not a security boundary).
	_ = unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")

	// Writable scratch. When the worktree does NOT live under /tmp we mount a private
	// tmpfs over /tmp — invisible to the host, mirroring the container's tmpfs — and
	// grant RW on all of it (it is this namespace's own /tmp). When the worktree DOES
	// live under /tmp (production: worktrees are os.MkdirTemp("","nilcore-wt-") →
	// /tmp/nilcore-wt-XXXX/<leaf>), mounting over /tmp would shadow the worktree, so
	// we cannot. There we must NOT grant RW to the shared host /tmp — that would let a
	// sandboxed command write into a SIBLING run's worktree/scratch under /tmp during
	// concurrent multi-agent runs (an I4 cross-run isolation hole). Instead we carve a
	// run-private scratch dir under the worktree's own run-scoped parent (removed by
	// worktree.Release) and grant RW to exactly that.
	scratch := sandboxScratch(workdir)
	if !underTmp(workdir) {
		_ = unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "")
	} else {
		// Best-effort create; if it fails the command simply has no extra scratch
		// beyond its worktree (fail-closed: less writable surface, never more).
		_ = os.MkdirAll(scratch, 0o700)
	}

	// SECURITY (I3+I4): mask the operator's credential paths. Landlock below is
	// allowlist-only — the {"/", read+exec} grant cannot SUBTRACT ~/.ssh or the
	// NilCore config dir (secrets.vault + secrets.key + secrets.salt live there:
	// readable together they decrypt every stored secret INSIDE the sandbox, at
	// the same uid). The fresh mount namespace can subtract: shadow each existing
	// credential path with an empty read-only mount. This must happen exactly
	// HERE — after the MS_PRIVATE remount (the masks never propagate back to the
	// host) and BEFORE Landlock and seccomp, because a Landlock-restricted thread
	// is denied all filesystem-topology changes and the seccomp filter denylists
	// mount(2), so a later mask would EPERM. Fail-closed like Landlock: a MOUNT
	// failure on a path that stat'd successfully aborts the exec (exit 126 via
	// MaybeRunInit) rather than run with a credential path exposed. A path we
	// cannot stat is skipped — it is already unreadable to the same-uid command.
	if err := maskSensitivePaths(buildMaskSet(maskHome, maskCfg, workdir)); err != nil {
		return fmt.Errorf("mask credential paths: %w", err)
	}

	if err := unix.Chdir(workdir); err != nil {
		return fmt.Errorf("chdir %s: %w", workdir, err)
	}

	// no_new_privs: prerequisite for unprivileged landlock_restrict_self, and it
	// blocks any setuid/setgid escalation inside the sandbox.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}

	// Read+execute the host toolchain everywhere (never writable); read+write only
	// the worktree and the /tmp scratch; read+write the usual character devices.
	rules := []landlockRule{
		{"/", landlockReadExec()},
		{workdir, landlockReadWrite()},
		{scratch, landlockReadWrite()},
		{"/dev/null", landlockDevRW()},
		{"/dev/zero", landlockDevRW()},
		{"/dev/full", landlockDevRW()},
		{"/dev/random", landlockDevRW()},
		{"/dev/urandom", landlockDevRW()},
		{"/dev/tty", landlockDevRW()},
	}
	if err := applyLandlock(rules); err != nil {
		return fmt.Errorf("apply landlock: %w", err)
	}

	// seccomp-bpf is the last layer, applied after Landlock and just before the
	// execve so the filter is carried into the command. Defense-in-depth on top of
	// the namespaces + Landlock that already satisfy I4; a kernel without seccomp
	// filtering degrades gracefully (applySeccomp returns nil).
	if err := applySeccomp(); err != nil {
		return fmt.Errorf("apply seccomp: %w", err)
	}
	return nil
}

// underTmp reports whether p is /tmp or beneath it — where a tmpfs mount would
// shadow the worktree.
func underTmp(p string) bool {
	return p == "/tmp" || strings.HasPrefix(p, "/tmp/")
}

// sandboxScratch is the single writable scratch path granted to the command beyond
// its worktree (and used as HOME / TMPDIR). When the worktree is not under /tmp the
// scratch is the private tmpfs at /tmp. When it IS under /tmp we cannot mount over
// /tmp, so the scratch is a run-private subdir of the worktree's parent — never the
// shared host /tmp — so concurrent runs cannot write into each other's space.
func sandboxScratch(workdir string) string {
	if !underTmp(workdir) {
		return "/tmp"
	}
	parent := filepath.Dir(workdir)
	// Guard against a degenerate workdir like "/tmp" or "/tmp/x" whose parent is "/"
	// or "/tmp": fall back to a dir beside the worktree rather than widening scope.
	if parent == "/" || parent == "/tmp" || parent == "." {
		return filepath.Join(workdir, ".nilcore-scratch")
	}
	return filepath.Join(parent, ".nilcore-scratch")
}

// nilcoreConfigDir mirrors internal/paths.ConfigDir — $XDG_CONFIG_HOME/nilcore
// (default ~/.config/nilcore) on Linux — the directory cmd/nilcore keeps the
// encrypted vault, its master-key file, and its salt in (secrets.vault /
// secrets.key / secrets.salt). Replicated as one stdlib call rather than
// imported: the sandbox leaf's documented import footprint is stdlib + x/sys
// (docs/ARCHITECTURE.md layer map), and widening it for a Join is not worth it.
func nilcoreConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config dir: %w", err)
	}
	return filepath.Join(base, "nilcore"), nil
}

// buildMaskSet returns the absolute host paths whose contents must never be
// readable by a model-emitted command: the NilCore config dir (encrypted vault
// + master key + salt — readable together they decrypt every stored secret)
// and the classic credential paths under the operator's home. The set is
// curated and concrete — no runtime globbing — because every mask is a mount
// and the failure mode of a bad pattern is an unusable sandbox. Nothing in the
// sandbox legitimately needs any of these: egress is deny-all, so e.g.
// git-over-ssh could not reach a remote anyway.
//
// A candidate that equals, contains, or is contained by the worktree is
// dropped — masking it would shadow the one tree the command is entitled to
// read and write. An operator who points the worktree at (or under) a
// credential path has made that exposure explicitly.
//
// Symlinks are resolved before that overlap check (resolveForOverlap): on a
// symlinked-home host (/home → /var/home) or when a mask target is itself a
// symlink, a lexical prefix test on the UNRESOLVED candidate would miss an
// overlap with the already-symlink-resolved workdir and plant a tmpfs over the
// worktree ⇒ chdir fails / worktree hidden ⇒ exit 126. Resolving both sides
// consistently makes the comparison — and the returned mask paths — reflect the
// real filesystem the mounts and Landlock actually see. Resolution is
// best-effort (EvalSymlinks, falling back to Clean) because a credential path
// need not exist; a non-existent target simply keeps its cleaned form.
//
// Not pure (best-effort EvalSymlinks I/O), but the resolution is a no-op on
// paths that don't exist or aren't symlinks, so the table tests below still
// exercise the full selection logic on any OS that can compile the file.
func buildMaskSet(home, configDir, workdir string) []string {
	var candidates []string
	if configDir != "" {
		candidates = append(candidates, configDir)
	}
	if home != "" {
		for _, rel := range []string{
			".ssh",                    // private keys, known_hosts
			".aws",                    // cloud credentials
			".gnupg",                  // signing keys
			".netrc",                  // machine/login/password triplets (curl, git)
			".git-credentials",        // git credential.helper=store: plaintext user/pass/PAT
			".config/git/credentials", // XDG variant of the git credential store
			".config/gh",              // GitHub CLI OAuth token
			".docker/config.json",     // registry auths
			".codex",                  // delegated-CLI credentials (Codex)
			".claude",                 // delegated-CLI credentials (Claude Code)
			".claude.json",            // delegated-CLI state (Claude Code)
		} {
			candidates = append(candidates, filepath.Join(home, rel))
		}
	}
	// The workdir arg is already symlink-resolved by the parent (newNamespace),
	// but resolve it again so the comparison is correct even if a caller passes an
	// unresolved path, and so both sides go through identical normalization.
	wd := resolveForOverlap(workdir)
	excludeWD := filepath.IsAbs(wd)
	seen := make(map[string]bool, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		c = resolveForOverlap(c)
		if !filepath.IsAbs(c) || seen[c] {
			continue
		}
		if excludeWD && (pathContains(wd, c) || pathContains(c, wd)) {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// resolveForOverlap normalizes a path for the workdir-overlap comparison and for
// the mask target itself: it follows symlinks so a symlinked home or a symlinked
// credential path compares (and later mounts) against its real location. It is
// best-effort — a path that does not exist yet cannot be EvalSymlinks'd, so it
// falls back to filepath.Clean. Non-absolute inputs are returned cleaned and are
// filtered out by the absolute-path guard in the caller.
func resolveForOverlap(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// pathContains reports whether child is parent or lies beneath it. Both paths
// must be Clean'd and absolute (Linux-only code, so "/" separators).
func pathContains(parent, child string) bool {
	if parent == child || parent == "/" {
		return true
	}
	return strings.HasPrefix(child, parent+"/")
}

// maskSensitivePaths shadows each EXISTING target with an empty read-only
// mount, subtracting it from the read-everywhere Landlock grant that follows:
//
//   - a directory gets a fresh read-only tmpfs: empty, nothing to list, and
//     MS_RDONLY at mount time so no remount step is needed;
//   - anything else (a regular file such as ~/.netrc) gets /dev/null
//     bind-mounted over it: reads hit EOF immediately and the device discards
//     writes. /dev/null is preferred over binding an empty regular file
//     because it always exists (no pre-Landlock file creation) and needs no
//     read-only remount — an MS_REMOUNT|MS_BIND in a user namespace can EPERM
//     when the source mount carries locked flags. Landlock (applied right
//     after, read+exec-only outside the worktree) independently denies writes
//     through the masked path.
//
// A missing target is skipped silently — there is nothing to protect (ENOTDIR
// means a parent component is a file, so the path cannot exist either).
// Targets are processed in order, so a candidate nested inside an
// already-masked directory stats as absent and is skipped, never
// double-mounted.
//
// ANY stat failure skips the target rather than aborting: a stat that fails
// with EACCES/EPERM (e.g. a root-owned mode-700 ~/.docker left by `sudo docker
// login`) means the path is unreachable to us — but the command runs at the
// SAME uid with the SAME credentials, so it cannot traverse or read it either.
// An unstatable path is therefore already unreadable to the command; skipping
// it is as safe as masking it, and failing closed there would brick the whole
// backend (exit 126) over a path the model was never going to reach. A stat
// error does not PROVE the target exists, so it does not trigger fail-closed.
//
// Fail-closed applies only to a genuine MOUNT failure on a path that DID stat
// successfully — a provably-existing credential target we could not mask. The
// caller then aborts the exec (exit 126 via MaybeRunInit), exactly like a
// Landlock failure, because the alternative is running with that path exposed.
func maskSensitivePaths(targets []string) error {
	for _, target := range targets {
		fi, err := os.Stat(target)
		if err != nil {
			// Unreachable-to-us (or absent): already unreadable to the command,
			// which shares our uid. Skip, with a one-line notice for the non-absent
			// cases so an operator can see a credential path went unmasked.
			if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
				fmt.Fprintf(os.Stderr, "nilcore sandbox: skip unstatable mask target %s: %v\n", target, err)
			}
			continue
		}
		if fi.IsDir() {
			if err := unix.Mount("tmpfs", target, "tmpfs",
				unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
				return fmt.Errorf("mask dir %s: %w", target, err)
			}
			continue
		}
		if err := unix.Mount("/dev/null", target, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("mask file %s: %w", target, err)
		}
	}
	return nil
}

// sandboxEnv is the command's environment: the inherited env minus our control
// vars, with a sane PATH and a writable HOME + Go caches pinned into the
// confined scratch dir (matching the container backend).
func sandboxEnv(home string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, envMarker+"=") ||
			strings.HasPrefix(kv, envWorkdir+"=") ||
			strings.HasPrefix(kv, envCmd+"=") ||
			strings.HasPrefix(kv, envMaskHome+"=") ||
			strings.HasPrefix(kv, envMaskCfg+"=") {
			continue
		}
		env = append(env, kv)
	}
	env = defaultEnv(env, "PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	env = setEnv(env, "HOME", home)
	// TMPDIR points at the writable scratch (== home). REQUIRED since the namespace
	// backend no longer grants RW to all of shared host /tmp when the worktree lives
	// under /tmp (the B3 isolation fix): a tool that writes scratch must reach the
	// run-private dir, not /tmp-at-large. In the non-/tmp case home IS the private
	// tmpfs /tmp, so this is a no-op there. TMP/TEMP set too for portability.
	env = setEnv(env, "TMPDIR", home)
	env = setEnv(env, "TMP", home)
	env = setEnv(env, "TEMP", home)
	env = setEnv(env, "GOCACHE", filepath.Join(home, ".gocache"))
	env = setEnv(env, "GOPATH", filepath.Join(home, ".gopath"))
	return env
}

func setEnv(env []string, key, val string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + val
			return env
		}
	}
	return append(env, prefix+val)
}

func defaultEnv(env []string, key, val string) []string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return env
		}
	}
	return append(env, prefix+val)
}

// detectNamespace reports whether this host can run the namespace backend: a
// Landlock-capable kernel plus usable unprivileged user namespaces. It is
// conservative — it prefers a false negative (fall back to a container) over a
// false positive (a backend that EPERMs at exec) — so `auto` stays correct on
// hosts that gate user namespaces behind AppArmor or a sysctl.
func detectNamespace() (bool, string) {
	abi, err := landlockABI()
	if err != nil || abi < 1 {
		return false, "no Landlock LSM (need Linux 5.13+ with landlock enabled in the kernel)"
	}
	if reason, ok := userNSUsable(); !ok {
		return false, reason
	}
	return true, ""
}

func userNSUsable() (string, bool) {
	if v, ok := readSysctl("/proc/sys/user/max_user_namespaces"); ok && v == "0" {
		return "unprivileged user namespaces disabled (user.max_user_namespaces=0)", false
	}
	if v, ok := readSysctl("/proc/sys/kernel/unprivileged_userns_clone"); ok && v == "0" {
		return "unprivileged user namespaces disabled (kernel.unprivileged_userns_clone=0)", false
	}
	// apparmor_restrict_unprivileged_userns is enabled for ANY nonzero value, not
	// just 1: newer kernels use higher levels (e.g. 2) for the same restriction, so
	// an ==1 test false-negatived those and let selection pick a backend that then
	// EPERMs at clone. Treat any nonzero (and any unparseable-but-present) value as
	// restricted, staying conservative (prefer a container fallback over a failing
	// namespace exec). A literal "0" is the only value that means unrestricted.
	if v, ok := readSysctl("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); ok && v != "0" {
		return "unprivileged user namespaces restricted by AppArmor (kernel.apparmor_restrict_unprivileged_userns=" + v + ")", false
	}
	return "", true
}

func readSysctl(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// landlockRule grants access on a path subtree (a file or a directory tree).
type landlockRule struct {
	path   string
	access uint64
}

// landlockABI returns the kernel's supported Landlock ABI version (>=1), or an
// error when Landlock is unavailable. Querying the version allocates no fd.
func landlockABI() (int, error) {
	r, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0,
		uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}

// applyLandlock creates a ruleset over every filesystem right the running
// kernel's ABI supports, grants each rule's access (narrowed to that ABI), and
// restricts the current thread. Anything not granted is denied — so read+exec
// on "/" plus read+write on the worktree yields exactly: read the host
// toolchain, write only inside the worktree.
func applyLandlock(rules []landlockRule) error {
	abi, err := landlockABI()
	if err != nil {
		return fmt.Errorf("query landlock abi: %w", err)
	}
	handled := landlockHandled(abi)

	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("create ruleset: %w", errno)
	}
	rulesetFD := int(fd)
	defer unix.Close(rulesetFD)

	for _, r := range rules {
		access := r.access & handled
		if access == 0 {
			continue
		}
		pathFD, err := unix.Open(r.path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			continue // a path that doesn't exist on this host simply grants nothing
		}
		pb := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(pathFD)}
		_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(rulesetFD),
			uintptr(unix.LANDLOCK_RULE_PATH_BENEATH), uintptr(unsafe.Pointer(&pb)), 0, 0, 0)
		_ = unix.Close(pathFD)
		if errno != 0 {
			return fmt.Errorf("add rule %s: %w", r.path, errno)
		}
	}

	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(rulesetFD), 0, 0); errno != 0 {
		return fmt.Errorf("restrict self: %w", errno)
	}
	return nil
}

// landlockHandled is the full set of filesystem rights to enforce, narrowed to
// what the kernel's ABI knows: requesting a bit the kernel doesn't recognize is
// an error, so REFER (ABI 2) and TRUNCATE (ABI 3) are added only when present.
func landlockHandled(abi int) uint64 {
	h := landlockReadExec() |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM
	if abi >= 2 {
		h |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		h |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	return h
}

func landlockReadExec() uint64 {
	return unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_EXECUTE
}

// landlockReadWrite is every right we ever enforce; applyLandlock masks it down
// to the kernel's ABI, so naming REFER/TRUNCATE here is safe on older kernels.
func landlockReadWrite() uint64 {
	return landlockReadExec() |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
		unix.LANDLOCK_ACCESS_FS_REFER | unix.LANDLOCK_ACCESS_FS_TRUNCATE
}

// landlockDevRW grants read+write to a single character device (e.g. /dev/null)
// without granting directory mutation under /dev.
func landlockDevRW() uint64 {
	return unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE
}
