//go:build linux && arm64

package sandbox

import "golang.org/x/sys/unix"

// seccompAuditArch is the AUDIT_ARCH value the BPF filter validates seccomp_data
// against (see the amd64 variant for the rationale).
const seccompAuditArch uint32 = unix.AUDIT_ARCH_AARCH64
