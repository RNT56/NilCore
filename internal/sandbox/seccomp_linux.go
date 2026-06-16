//go:build linux

// seccomp-bpf is the namespace backend's third confinement layer (P7-T02), added
// in the same re-exec child as no_new_privs + Landlock and applied LAST, just
// before execve. It is defense-in-depth: namespaces + Landlock already satisfy
// I4 on their own, so a kernel without seccomp filtering degrades gracefully
// (the command still runs confined). The filter is a DENYLIST — it blocks a
// curated set of clearly-dangerous syscalls (mount, ptrace, kexec, module
// (un)load, namespace manipulation, the kernel keyring, clock setting, …) with
// EPERM and allows everything else, so ordinary build/test toolchains keep
// working while the kernel attack surface shrinks. An allowlist (P7 follow-up)
// would be stronger but risks breaking the long tail of toolchain syscalls.
package sandbox

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// seccomp_data field byte offsets (uapi/linux/seccomp.h): nr is a 32-bit int at
// offset 0, arch a 32-bit value at offset 4. The BPF program reads both as words.
const (
	seccompOffsetNR   = 0
	seccompOffsetArch = 4
)

// seccompDenied is the set of syscalls the sandbox refuses. Every entry resolves
// to the running GOARCH's syscall number via unix.SYS_* (so the same list is
// correct on amd64 and arm64). The command never needs these; they are the
// escalation / persistence / sandbox-escape primitives.
func seccompDenied() []uint32 {
	return []uint32{
		unix.SYS_MOUNT, unix.SYS_UMOUNT2, // mounting (already done pre-seccomp)
		unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT, // root pivots
		unix.SYS_SETNS, unix.SYS_UNSHARE, // namespace manipulation / nesting
		unix.SYS_PTRACE,                                                     // process inspection/injection
		unix.SYS_KEXEC_LOAD,                                                 // load a new kernel
		unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE, // kernel modules
		unix.SYS_REBOOT,                   // reboot/halt
		unix.SYS_SWAPON, unix.SYS_SWAPOFF, // swap
		unix.SYS_BPF,                                            // loading BPF (could weaken the sandbox)
		unix.SYS_PERF_EVENT_OPEN,                                // perf
		unix.SYS_ADD_KEY, unix.SYS_KEYCTL, unix.SYS_REQUEST_KEY, // kernel keyring
		unix.SYS_ACCT,                            // process accounting
		unix.SYS_SETTIMEOFDAY, unix.SYS_ADJTIMEX, // clock
		unix.SYS_CLOCK_SETTIME, unix.SYS_CLOCK_ADJTIME,
		unix.SYS_QUOTACTL,                                     // disk quotas
		unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, // cross-process memory
	}
}

// applySeccomp installs the denylist filter on the current thread (and all
// threads, via TSYNC). It is fail-closed on a malformed filter but graceful when
// the kernel has no seccomp filtering at all (ENOSYS → nil): namespaces +
// Landlock still confine the command. seccompAuditArch == 0 (an arch we don't
// target) likewise skips the filter.
func applySeccomp() error {
	if seccompAuditArch == 0 {
		return nil
	}
	filter := seccompProgram(seccompDenied())
	fprog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}

	// Prefer seccomp(2) with TSYNC so every thread shares the filter. The filter
	// is carried across the upcoming execve regardless.
	_, _, errno := unix.Syscall(uintptr(unix.SYS_SECCOMP),
		uintptr(unix.SECCOMP_SET_MODE_FILTER), uintptr(unix.SECCOMP_FILTER_FLAG_TSYNC),
		uintptr(unsafe.Pointer(&fprog)))
	if errno == 0 {
		return nil
	}
	if errno == unix.ENOSYS {
		// Very old kernel without the seccomp(2) syscall: try the prctl path
		// (no TSYNC). Still ENOSYS/EINVAL there ⇒ no seccomp filtering available;
		// degrade gracefully rather than refuse to run.
		if err := unix.Prctl(unix.PR_SET_SECCOMP, uintptr(unix.SECCOMP_MODE_FILTER),
			uintptr(unsafe.Pointer(&fprog)), 0, 0); err != nil {
			if err == unix.ENOSYS || err == unix.EINVAL {
				return nil
			}
			return err
		}
		return nil
	}
	return errno
}

// seccompProgram builds the classic-BPF program: validate the arch (kill on a
// mismatch — defends against syscall-number confusion across personalities),
// then for each denied syscall return EPERM, else allow. Jump offsets are
// computed from positions so the program is correct by construction.
func seccompProgram(deny []uint32) []unix.SockFilter {
	n := len(deny)
	// Layout (indices): 0 load arch · 1 arch-check · 2 load nr · 3..3+n-1 denied
	// checks · n+3 ALLOW · n+4 DENY · n+5 KILL.  BPF jump target = pc+1+offset.
	prog := make([]unix.SockFilter, 0, n+6)
	prog = append(prog, bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompOffsetArch))
	// arch matches ⇒ fall through to the nr load; mismatch ⇒ jump to KILL (n+5).
	prog = append(prog, bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, seccompAuditArch, 0, uint8(n+3)))
	prog = append(prog, bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompOffsetNR))
	for j, nr := range deny {
		// at index 3+j; jump to DENY (n+4) on a match ⇒ jt = (n+4)-(3+j)-1 = n-j.
		prog = append(prog, bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, nr, uint8(n-j), 0))
	}
	prog = append(prog, bpfStmt(unix.BPF_RET|unix.BPF_K, uint32(unix.SECCOMP_RET_ALLOW)))
	prog = append(prog, bpfStmt(unix.BPF_RET|unix.BPF_K,
		uint32(unix.SECCOMP_RET_ERRNO)|(uint32(unix.EPERM)&uint32(unix.SECCOMP_RET_DATA))))
	prog = append(prog, bpfStmt(unix.BPF_RET|unix.BPF_K, uint32(unix.SECCOMP_RET_KILL_PROCESS)))
	return prog
}

func bpfStmt(code uint16, k uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: k}
}

func bpfJump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}
