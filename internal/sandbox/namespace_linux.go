//go:build linux

// The namespace backend confines a model-emitted command (invariant I4) with
// Linux kernel primitives instead of a container runtime: it is born inside
// fresh user/mount/pid/net/ipc/uts namespaces, then — in the re-exec'd child,
// before the command runs — sets no_new_privs, a Landlock ruleset that maps
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
	"unsafe"

	"golang.org/x/sys/unix"
)

// Control vars the parent sets on the re-exec'd child. They tell MaybeRunInit
// what to confine and run; they are stripped from the command's own environment.
const (
	envMarker  = "NILCORE_SANDBOX_INIT"
	envWorkdir = "NILCORE_SANDBOX_WORKDIR"
	envCmd     = "NILCORE_SANDBOX_CMD"
)

// Namespace runs each command in a throwaway set of Linux namespaces with a
// Landlock filesystem domain. It needs no container runtime, image, or daemon.
type Namespace struct {
	HostDir string // absolute, symlink-resolved path to the worktree
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
func (n *Namespace) ExecWithEnv(ctx context.Context, cmd string, env map[string]string) (Result, error) {
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

	if err := sandboxInit(os.Getenv(envWorkdir)); err != nil {
		fmt.Fprintf(os.Stderr, "nilcore sandbox init: %v\n", err)
		os.Exit(126)
	}
	argv := []string{"/bin/sh", "-c", os.Getenv(envCmd)}
	if err := unix.Exec("/bin/sh", argv, sandboxEnv(sandboxScratch(os.Getenv(envWorkdir)))); err != nil {
		fmt.Fprintf(os.Stderr, "nilcore sandbox exec: %v\n", err)
		os.Exit(127)
	}
}

// sandboxInit establishes the confinement. The security boundaries (no_new_privs,
// Landlock) are fail-closed; cosmetic steps (private mounts, a fresh /proc) are
// best-effort — Landlock, not /proc, is the boundary.
func sandboxInit(workdir string) error {
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

// sandboxEnv is the command's environment: the inherited env minus our control
// vars, with a sane PATH and a writable HOME + Go caches pinned into the
// confined scratch dir (matching the container backend).
func sandboxEnv(home string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, envMarker+"=") ||
			strings.HasPrefix(kv, envWorkdir+"=") ||
			strings.HasPrefix(kv, envCmd+"=") {
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
	if v, ok := readSysctl("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); ok && v == "1" {
		return "unprivileged user namespaces restricted by AppArmor (kernel.apparmor_restrict_unprivileged_userns=1)", false
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
