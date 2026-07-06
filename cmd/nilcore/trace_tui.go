//go:build tui

package main

import "nilcore/internal/trace"

// runTraceExplorer launches the interactive bubbletea trace explorer (built only
// under the `tui` tag, so the default binary links zero Charm — invariant I6). The
// non-tui build gets the stub in trace_tui_stub.go.
func runTraceExplorer(tr *trace.Trace) error {
	return trace.RunExplorer(tr)
}
