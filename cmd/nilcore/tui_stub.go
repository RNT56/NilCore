//go:build !tui

package main

import (
	"fmt"
	"os"
)

// tuiMain is the default (Charm-free) build's stub for `nilcore tui`. The
// full-screen TUI is an OPT-IN build so the default binary stays dependency-free:
// only the `tui`-tagged build links the Charm stack (bubbletea/lipgloss/bubbles),
// keeping the core zero-dependency (invariant I6). `nilcore chat` is the full
// streaming conversational front door in the default build.
func tuiMain(_ []string) {
	fmt.Fprintln(os.Stderr, "the full-screen TUI is an opt-in build (keeps the default binary dependency-free):")
	fmt.Fprintln(os.Stderr, "  go build -tags tui -o nilcore-tui ./cmd/nilcore   # then: ./nilcore-tui tui")
	fmt.Fprintln(os.Stderr, "  (or grab the prebuilt nilcore-tui binary from Releases)")
	fmt.Fprintln(os.Stderr, "meanwhile, `nilcore chat` is the full streaming conversational front door.")
	os.Exit(2)
}
