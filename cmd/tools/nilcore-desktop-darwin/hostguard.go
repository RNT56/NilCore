package main

import (
	"context"
	"os"
	"strings"
)

// This file is the CU-MAC-T09 host-mode SCREENSHOT mitigation (ROADMAP-COMPUTER-USE-
// DARWIN.md §0.3/§3): "exclude the controlling terminal from screenshots". The
// terminal that launched the agent renders the human-gate approval prompt and may
// show secrets; feeding its pixels to the model is both an I7 instruction-injection
// surface and an I3 leak surface. The cliclick MVP shells `screencapture -x` (a whole-
// display grab with no window-exclusion flag — screencapture cannot subtract a
// window), so the faithful mitigation here is FAIL-CLOSED: when the controlling
// terminal is the frontmost window, refuse to capture rather than send its contents.
// A true per-window redaction belongs to the signed helper (CU-MAC-T05), tracked
// alongside the CGEvent source-userData tagging (see hostModeNotes below).

// controllingTerminal returns the agent's own terminal application name, from the
// terminal-set $TERM_PROGRAM env (Apple_Terminal, iTerm.app, vscode, …). It reads our
// OWN process env — not page/screen content — so it is trusted input, not I7 data.
// Empty when unknown (e.g. a non-terminal launch context).
func controllingTerminal() string {
	return strings.TrimSpace(os.Getenv("TERM_PROGRAM"))
}

// terminalIsFrontmost reports whether the frontmost app is (heuristically) the
// controlling terminal. It compares the AppleScript frontmost-app name against
// $TERM_PROGRAM with the common name aliases macOS uses (Terminal ⇄ Apple_Terminal,
// iTerm ⇄ iTerm.app). Frontmost is read via the same osascript seam the allowlist
// uses; on failure it returns false (the capture proceeds — fail-closed would block
// every capture if frontmost can never be read, which is too aggressive for the
// screenshot path, unlike the actuation path where guardMutation already fails closed).
func terminalIsFrontmost(ctx context.Context) bool {
	term := controllingTerminal()
	if term == "" {
		return false
	}
	front := strings.TrimSpace(frontmostApp(ctx))
	if front == "" {
		return false
	}
	return terminalNamesMatch(term, front)
}

// terminalNamesMatch normalizes the two macOS terminal-name forms and compares.
func terminalNamesMatch(termProgram, frontmost string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.TrimSuffix(s, ".app")
		s = strings.ReplaceAll(s, "apple_", "")
		s = strings.ReplaceAll(s, "_", "")
		s = strings.ReplaceAll(s, " ", "")
		return s
	}
	a, b := norm(termProgram), norm(frontmost)
	if a == "" || b == "" {
		return false
	}
	// Exact match, or one is a PREFIX of the other (iterm ⇄ iterm2) — a prefix avoids the
	// false positive a substring check would hit (e.g. "code" ⊂ "vscode").
	return a == b || strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// hostModeNotes are the recorded host-mode limitations surfaced once at startup (the
// banner) and in the probe: the screenshot terminal-exclusion is fail-closed-only (no
// per-window redaction in the cliclick MVP), and synthetic CGEvents are NOT yet
// source-userData-tagged (that needs the signed helper, CU-MAC-T05). Recording the gap
// honestly is the §3 requirement until the helper ships.
func hostModeNotes() []string {
	return []string{
		"screenshots are whole-display: when the controlling terminal is frontmost the capture is REFUSED (it shows the approval prompt/secrets) — keep the terminal in the background or on another Space; per-window redaction is the signed-helper tier (CU-MAC-T05).",
		"synthetic input is NOT yet source-userData-tagged (human-vs-agent input is indistinguishable to other tools) — that tagging also requires the signed CGEvent helper (CU-MAC-T05).",
	}
}
