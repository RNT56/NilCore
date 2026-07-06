//go:build !tui

package main

import (
	"fmt"

	"nilcore/internal/trace"
)

// runTraceExplorer is the default-build stub: the interactive explorer links the
// Charm stack, which only the `tui` build tag pulls in (invariant I6 — the default
// binary links zero Charm). It errors with the actionable rebuild instruction.
func runTraceExplorer(*trace.Trace) error {
	return fmt.Errorf("the interactive trace explorer needs the TUI build — rebuild with 'make tui' (or 'go build -tags tui')")
}
