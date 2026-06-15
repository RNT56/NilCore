//go:build linux

package sandbox

import (
	"strings"
	"testing"
)

// TestNamespaceSeccompActive proves the filter is installed: /proc/<pid>/status
// reports Seccomp mode 2 (SECCOMP_MODE_FILTER) for the sandboxed command. This
// also confirms the whole existing TestNamespace* suite runs WITH seccomp active
// (it's wired into sandboxInit), so those tests double as the "normal syscalls
// still work under the filter" regression.
func TestNamespaceSeccompActive(t *testing.T) {
	box := requireNamespace(t)
	res := runSandbox(t, box, "grep '^Seccomp:' /proc/self/status")
	if !strings.Contains(res.Stdout, "2") {
		t.Fatalf("expected Seccomp mode 2 (filter) for the sandboxed command, got %q (stderr %q)", res.Stdout, res.Stderr)
	}
}

// TestNamespaceSeccompBlocksChroot proves the denylist actually denies: chroot is
// a CAP_SYS_CHROOT operation the sandbox holds inside its user namespace, so
// WITHOUT seccomp `chroot / /bin/true` would succeed (rc=0). The filter turns the
// chroot syscall into EPERM, so the command must fail (rc!=0).
func TestNamespaceSeccompBlocksChroot(t *testing.T) {
	box := requireNamespace(t)
	res := runSandbox(t, box, `chroot / /bin/true 2>/dev/null; echo "rc=$?"`)
	if strings.Contains(res.Stdout, "rc=0") {
		t.Fatalf("SECCOMP BREACH: chroot succeeded inside the sandbox (output %q)", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "rc=") {
		t.Fatalf("unexpected output %q (stderr %q)", res.Stdout, res.Stderr)
	}
}

// TestNamespaceSeccompAllowsNormalWork is an explicit guard that the denylist
// doesn't block the ordinary syscalls a build/test command needs: spawn a child,
// write a file, read it back, pipe.
func TestNamespaceSeccompAllowsNormalWork(t *testing.T) {
	box := requireNamespace(t)
	res := runSandbox(t, box, "printf 'a\\nb\\nc\\n' > f && sort f | tr -d '\\n'")
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "abc" {
		t.Fatalf("normal toolchain syscalls should work under seccomp: exit %d out %q stderr %q", res.ExitCode, res.Stdout, res.Stderr)
	}
}

// TestSeccompProgramShape is a hermetic, OS-independent check of the BPF jump
// arithmetic: the program is arch-check + nr-load + one test per denied syscall +
// three return rows, and the arch mismatch jumps to KILL.
func TestSeccompProgramShape(t *testing.T) {
	deny := []uint32{10, 20, 30}
	prog := seccompProgram(deny)
	if got, want := len(prog), len(deny)+6; got != want {
		t.Fatalf("program length = %d, want %d", got, want)
	}
	// Index 1 is the arch check; its jf must land on the final instruction (KILL).
	archJump := prog[1]
	killIndex := 1 + 1 + int(archJump.Jf)
	if killIndex != len(prog)-1 {
		t.Fatalf("arch mismatch jumps to index %d, want KILL at %d", killIndex, len(prog)-1)
	}
	// Each denied test (indices 3..3+n-1) must jump on match to the DENY row at n+4.
	denyIndex := len(deny) + 4
	for j := range deny {
		pc := 3 + j
		target := pc + 1 + int(prog[pc].Jt)
		if target != denyIndex {
			t.Fatalf("denied test %d jumps to index %d, want DENY at %d", j, target, denyIndex)
		}
	}
}
