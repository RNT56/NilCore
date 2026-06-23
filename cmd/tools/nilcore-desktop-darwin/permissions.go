package main

import (
	"context"
	"os/exec"
	"strings"
)

// This file is CU-MAC-T07: a LIVE permission/onboarding probe. macOS caches a
// process's TCC grants at launch, so the cheap `AXIsProcessTrusted` answers from a
// stale cache and a freshly-granted permission needs a restart. The probe instead
// checks LIVE BEHAVIOUR — it actually attempts a screencapture and inspects the
// outcome — so it never reports a false positive, and it emits actionable
// "grant X in System Settings, then restart your terminal" guidance.

// PermissionReport is the probe's structured result. Ready is the AND of the grants
// the host-control driver actually needs.
type PermissionReport struct {
	ScreenRecording   bool     `json:"screen_recording"`   // screencapture produced a real frame
	CliclickInstalled bool     `json:"cliclick_installed"` // the actuation binary is on PATH
	Ready             bool     `json:"ready"`              // all hard requirements satisfied
	Guidance          []string `json:"guidance"`           // human, actionable, ordered
}

// lookPath is the PATH-lookup seam (a var so tests can fake a missing/present tool).
var lookPath = exec.LookPath

// probePermissions runs the live checks. Screen Recording is verified by a real
// capture (the `could not create image from display` failure is the ungranted
// signature, confirmed empirically); cliclick is verified by presence on PATH
// (Accessibility itself can only be confirmed by an actuation, which would move the
// cursor — so the probe reports presence and guides the operator to grant it).
func probePermissions(ctx context.Context) PermissionReport {
	var r PermissionReport

	if _, err := capture(ctx); err == nil {
		r.ScreenRecording = true
	} else {
		r.Guidance = append(r.Guidance,
			"Screen Recording is OFF (screencapture cannot read the display). Grant it: System Settings ▸ Privacy & Security ▸ Screen Recording ▸ enable your terminal, then RESTART the terminal.")
	}

	if _, err := lookPath("cliclick"); err == nil {
		r.CliclickInstalled = true
		r.Guidance = append(r.Guidance,
			"cliclick is installed. Ensure Accessibility is granted: System Settings ▸ Privacy & Security ▸ Accessibility ▸ enable your terminal (clicks/keystrokes fail closed without it).")
	} else {
		r.Guidance = append(r.Guidance,
			"cliclick is NOT installed (clicks/typing will fail closed). Install it: brew install cliclick.")
	}

	r.Ready = r.ScreenRecording && r.CliclickInstalled
	if r.Ready {
		r.Guidance = append([]string{"All hard requirements satisfied — host control can run (confirm Accessibility is granted if actuation fails)."}, r.Guidance...)
	}
	return r
}

// String renders the report as a human block for `--probe` output.
func (r PermissionReport) String() string {
	var b strings.Builder
	b.WriteString("nilcore-desktop-darwin permission probe\n")
	b.WriteString("  Screen Recording : " + yesNo(r.ScreenRecording) + "\n")
	b.WriteString("  cliclick present : " + yesNo(r.CliclickInstalled) + "\n")
	b.WriteString("  ready            : " + yesNo(r.Ready) + "\n")
	for _, g := range r.Guidance {
		b.WriteString("  • " + g + "\n")
	}
	return b.String()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
