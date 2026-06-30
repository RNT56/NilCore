package main

import (
	"context"
	"errors"
	"image"
	"os"
	"path/filepath"
	"testing"

	"nilcore/internal/desktopwire"
)

func TestProbePermissions(t *testing.T) {
	oc, ol, oo := capture, lookPath, osascriptDesktopWidth
	defer func() { capture, lookPath, osascriptDesktopWidth = oc, ol, oo }()
	// Fake the osascript desktop-width probe so the test never shells the REAL osascript
	// (which can block on an Automation TCC prompt). A 1512-pt desktop with the 1x1
	// capture below yields a derivable scale, keeping this hermetic.
	osascriptDesktopWidth = func(context.Context) int { return 1512 }
	t.Setenv("NILCORE_MAC_SCALE", "")

	// Both grants present → ready.
	capture = func(context.Context) (image.Image, error) { return image.NewRGBA(image.Rect(0, 0, 1, 1)), nil }
	lookPath = func(string) (string, error) { return "/usr/local/bin/cliclick", nil }
	if r := probePermissions(context.Background()); !r.Ready || !r.ScreenRecording || !r.CliclickInstalled {
		t.Fatalf("expected ready, got %+v", r)
	}

	// Screen Recording denied (capture fails) → not ready, with guidance.
	capture = func(context.Context) (image.Image, error) {
		return nil, errors.New("could not create image from display")
	}
	r := probePermissions(context.Background())
	if r.Ready || r.ScreenRecording {
		t.Fatalf("expected not-ready when capture fails: %+v", r)
	}
	if len(r.Guidance) == 0 {
		t.Fatal("expected guidance when a grant is missing")
	}

	// cliclick missing → not ready.
	capture = func(context.Context) (image.Image, error) { return image.NewRGBA(image.Rect(0, 0, 1, 1)), nil }
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if r := probePermissions(context.Background()); r.Ready || r.CliclickInstalled {
		t.Fatalf("expected not-ready when cliclick missing: %+v", r)
	}
}

func TestKillSwitch(t *testing.T) {
	dir := t.TempDir()
	stop := filepath.Join(dir, "STOP")
	t.Setenv("NILCORE_DESKTOP_STOP", stop)

	if tripped, _ := killSwitchTripped(); tripped {
		t.Fatal("kill-switch should be inactive with no sentinel")
	}
	if err := guardMutation(context.Background()); err != nil {
		t.Fatalf("guardMutation should pass with no sentinel: %v", err)
	}
	// Trip it.
	if err := os.WriteFile(stop, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tripped, _ := killSwitchTripped(); !tripped {
		t.Fatal("kill-switch should be active once the sentinel exists")
	}
	if err := guardMutation(context.Background()); err == nil {
		t.Fatal("guardMutation must fail closed while the kill-switch is active")
	}
}

func TestAppAllowlist(t *testing.T) {
	of := frontmostApp
	defer func() { frontmostApp = of }()
	t.Setenv("NILCORE_DESKTOP_STOP", filepath.Join(t.TempDir(), "none")) // no kill-switch

	// No allowlist → always allowed.
	t.Setenv("NILCORE_DESKTOP_ALLOW_APPS", "")
	frontmostApp = func(context.Context) string { return "Terminal" }
	if err := guardMutation(context.Background()); err != nil {
		t.Fatalf("no allowlist should allow any app: %v", err)
	}

	// Allowlist with a match → allowed (case-insensitive).
	t.Setenv("NILCORE_DESKTOP_ALLOW_APPS", "Notes, Calculator")
	frontmostApp = func(context.Context) string { return "notes" }
	if err := guardMutation(context.Background()); err != nil {
		t.Fatalf("allowed app should pass: %v", err)
	}

	// Frontmost app not in allowlist → refused.
	frontmostApp = func(context.Context) string { return "Safari" }
	if err := guardMutation(context.Background()); err == nil {
		t.Fatal("an app outside the allowlist must be refused")
	}

	// Unidentifiable frontmost app under a non-empty allowlist → fail closed.
	frontmostApp = func(context.Context) string { return "" }
	if err := guardMutation(context.Background()); err == nil {
		t.Fatal("an unidentified frontmost app must fail closed under an allowlist")
	}
}

func TestGuardMutationWiredIntoPerform(t *testing.T) {
	// A mutating act is refused when the kill-switch is active; observe is not gated.
	dir := t.TempDir()
	stop := filepath.Join(dir, "STOP")
	t.Setenv("NILCORE_DESKTOP_STOP", stop)
	_ = os.WriteFile(stop, []byte("1"), 0o600)

	d := &driver{idBox: map[int]image.Rectangle{}, scaleX: 1, scaleY: 1, bscale: 2}
	if err := d.perform(context.Background(), desktopwire.Act{Op: desktopwire.OpClick, Coordinate: []int{10, 10}}); err == nil {
		t.Fatal("a click must be refused while the kill-switch is active")
	}
	if err := d.perform(context.Background(), desktopwire.Act{Op: desktopwire.OpObserve}); err != nil {
		t.Fatalf("observe must never be gated: %v", err)
	}
}

func TestTerminalNamesMatch(t *testing.T) {
	cases := []struct {
		term, front string
		want        bool
	}{
		{"Apple_Terminal", "Terminal", true},
		{"iTerm.app", "iTerm2", true}, // prefix-match (iterm ⇄ iterm2)
		{"iTerm.app", "iTerm", true},
		{"vscode", "Code", false},
		{"Apple_Terminal", "Safari", false},
		{"", "Terminal", false},
		{"Terminal", "", false},
	}
	for _, c := range cases {
		if got := terminalNamesMatch(c.term, c.front); got != c.want {
			t.Errorf("terminalNamesMatch(%q,%q) = %v, want %v", c.term, c.front, got, c.want)
		}
	}
}

func TestTerminalExclusionRefusesCapture(t *testing.T) {
	oc, of := capture, frontmostApp
	defer func() { capture, frontmostApp = oc, of }()
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	t.Setenv("NILCORE_MAC_SCALE", "")
	frontmostApp = func(context.Context) string { return "Terminal" } // the controlling terminal is up front
	captureCalled := false
	capture = func(context.Context) (image.Image, error) {
		captureCalled = true
		return image.NewRGBA(image.Rect(0, 0, 100, 100)), nil
	}

	d := &driver{idBox: map[int]image.Rectangle{}, scaleX: 1, scaleY: 1, bscale: 1}
	obs := d.observe(context.Background())
	if captureCalled {
		t.Fatal("capture must be REFUSED (never shelled) when the controlling terminal is frontmost")
	}
	if obs.Rung != desktopwire.RungCoordinate || len(obs.Console) == 0 {
		t.Fatalf("expected a refused-capture observation with a console note, got rung=%d console=%v", obs.Rung, obs.Console)
	}
	if obs.ScreenshotB64 != "" {
		t.Fatal("a refused capture must not carry a screenshot")
	}
}

func TestTerminalExclusionAllowsWhenTargetFrontmost(t *testing.T) {
	oc, of, oo, ob := capture, frontmostApp, osascriptDesktopWidth, backingScale
	defer func() { capture, frontmostApp, osascriptDesktopWidth, backingScale = oc, of, oo, ob }()
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	t.Setenv("NILCORE_MAC_SCALE", "1")
	frontmostApp = func(context.Context) string { return "TextEdit" } // a target app, not the terminal
	capture = func(context.Context) (image.Image, error) { return image.NewRGBA(image.Rect(0, 0, 100, 100)), nil }
	osascriptDesktopWidth = func(context.Context) int { return 100 }
	backingScale = func(context.Context, int) float64 { return 1 }

	d := &driver{idBox: map[int]image.Rectangle{}, scaleX: 1, scaleY: 1, bscale: 1}
	obs := d.observe(context.Background())
	// A blank capture yields Rung 3 with a screenshot — the point is it was NOT refused
	// for the terminal-exclusion reason.
	for _, c := range obs.Console {
		if c != "" && (containsAny(c, "capture refused")) {
			t.Fatalf("capture must NOT be refused when a target app is frontmost: %q", c)
		}
	}
}
