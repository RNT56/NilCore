//go:build linux && !amd64 && !arm64

package sandbox

// seccompAuditArch == 0 disables the seccomp layer on Linux architectures NilCore
// does not target (it ships for amd64 and arm64). applySeccomp returns nil when
// it is zero, so the namespace backend still confines the command with user
// namespaces + Landlock — just without the extra syscall filter.
const seccompAuditArch uint32 = 0
