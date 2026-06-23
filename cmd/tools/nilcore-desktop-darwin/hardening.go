package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// This file is CU-MAC-T09 host-control hardening: a KILL-SWITCH and a PER-APP
// ALLOWLIST, both fail-closed, evaluated before EVERY mutating act (click/type/key/
// scroll) on the real desktop. They narrow the relaxed-I4 host-control tier: the
// operator can halt instantly (touch a sentinel file) and can pin the agent to a set
// of apps, so a stray click can never escape into, say, the shell or a banking app.

// killSwitchPath is the operator's STOP sentinel: NILCORE_DESKTOP_STOP, else
// ~/.nilcore/desktop/STOP. While it exists, every mutation is refused.
func killSwitchPath() string {
	if p := strings.TrimSpace(os.Getenv("NILCORE_DESKTOP_STOP")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "nilcore-desktop-STOP")
	}
	return filepath.Join(home, ".nilcore", "desktop", "STOP")
}

func killSwitchTripped() (bool, string) {
	p := killSwitchPath()
	if _, err := os.Stat(p); err == nil {
		return true, p
	}
	return false, p
}

// allowedApps parses the comma-separated NILCORE_DESKTOP_ALLOW_APPS. Empty ⇒ no
// app restriction (the session gate already governed the run).
func allowedApps() []string {
	raw := strings.TrimSpace(os.Getenv("NILCORE_DESKTOP_ALLOW_APPS"))
	if raw == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// appAllowed reports whether the frontmost app is permitted by the allowlist. With
// no allowlist it is always true; with one, an unknown/empty frontmost app fails
// closed (refused), because acting blind into an unidentified window is exactly what
// the allowlist exists to prevent.
func appAllowed(ctx context.Context) (bool, string) {
	allow := allowedApps()
	if len(allow) == 0 {
		return true, ""
	}
	app := frontmostApp(ctx)
	for _, a := range allow {
		if strings.EqualFold(a, app) {
			return true, app
		}
	}
	return false, app
}

// guardMutation gates a mutating act. A non-nil error means REFUSE (fail closed).
func guardMutation(ctx context.Context) error {
	if tripped, p := killSwitchTripped(); tripped {
		return fmt.Errorf("kill-switch active (%s exists) — host control halted; remove the file to resume", p)
	}
	if ok, app := appAllowed(ctx); !ok {
		if app == "" {
			return fmt.Errorf("frontmost app could not be identified and an app allowlist is set — refusing to act")
		}
		return fmt.Errorf("frontmost app %q is not in NILCORE_DESKTOP_ALLOW_APPS — refusing to act", app)
	}
	return nil
}

// frontmostApp is the live seam reading the frontmost application name via
// AppleScript (no CGO). It may need Automation permission; on any failure it returns
// "" (which fails closed under a non-empty allowlist). A var so tests fake it.
var frontmostApp = func(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "osascript", "-e",
		`tell application "System Events" to name of first application process whose frontmost is true`).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
