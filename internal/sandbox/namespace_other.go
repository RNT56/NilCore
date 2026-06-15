//go:build !linux

package sandbox

import (
	"fmt"
	"runtime"
)

// detectNamespace always reports unsupported off Linux: Landlock and user
// namespaces are Linux-kernel features. Selection therefore falls back to the
// container backend on macOS/Windows.
func detectNamespace() (bool, string) {
	return false, fmt.Sprintf("namespace sandbox requires Linux (this host is %s)", runtime.GOOS)
}

// newNamespace is unavailable off Linux. Selection only reaches it when
// detectNamespace reported support, so this is a defensive guard.
func newNamespace(string) (Sandbox, error) {
	return nil, fmt.Errorf("namespace sandbox requires Linux (this host is %s)", runtime.GOOS)
}

// MaybeRunInit is a no-op off Linux: the re-exec sandbox init is Linux-only.
// cmd/nilcore calls it unconditionally at startup, so it must exist everywhere.
func MaybeRunInit() {}
