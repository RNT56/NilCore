package graapprove

import (
	"os"
	"path/filepath"
)

// defaultKillSwitchPath is the sentinel file that disables ALL auto-approval the
// instant it exists, with no restart (mirrors the --mac-host kill-switch). It is
// relative to the GradedApprover's root (normally the worktree).
const defaultKillSwitchPath = ".nilcore/AUTOAPPROVE_OFF"

// killSwitchEnv, when set to "1", disables auto-approval globally regardless of the
// sentinel file.
const killSwitchEnv = "NILCORE_AUTOAPPROVE_OFF"

// killSwitchEngaged reports whether the operator has tripped the kill-switch by
// either the environment variable (NILCORE_AUTOAPPROVE_OFF=1) or the sentinel file.
// root is the directory the sentinel path is resolved against (empty ⇒ relative to
// the process cwd). This is checked FIRST on every decision so revocation is
// instant.
func killSwitchEngaged(root, sentinel string) bool {
	if os.Getenv(killSwitchEnv) == "1" {
		return true
	}
	path := sentinel
	if path == "" {
		path = defaultKillSwitchPath
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	if _, err := os.Stat(path); err == nil {
		return true
	}
	return false
}
