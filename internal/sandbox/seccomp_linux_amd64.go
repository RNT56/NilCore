//go:build linux && amd64

package sandbox

import "golang.org/x/sys/unix"

// seccompAuditArch is the AUDIT_ARCH value the BPF filter validates seccomp_data
// against, so a syscall issued under a different personality (e.g. x86 on an
// x86_64 kernel) can't slip past with a confused syscall number.
const seccompAuditArch uint32 = unix.AUDIT_ARCH_X86_64
