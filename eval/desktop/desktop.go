// Package desktop is the desktop computer-use evaluation harness (Phase CU, CU-T10,
// the improvement-engine flywheel). It REUSES the eval/browse pure harness unchanged
// — the same FaultPlan, Scenario/Grade, and pass@1 / pass^k reliability math — and
// only adds the desktop-specific scenario catalog and fault taxonomy. Keeping the
// scoring logic identical to browse means the reliability numbers are comparable and
// themselves verifiable; only the test data differs.
//
// Every change to the Set-of-Marks ladder, the classical-CV detector, or the AT-SPI
// coverage is gated on this harness (pass@1 = capability, pass^k = reliability under
// faults) before it ships — realizing principle #9 (earn improvement from evidence).
// The LIVE runner — driving `nilcore desktop` against the sandbox image under fault
// injection — is the CI-only seam (no X11 on a dev host); these pure pieces drive it.
package desktop

import "nilcore/eval/browse"

// Desktop-specific faults (WAREX-flavoured, for a GUI rather than a network). They
// are browse.FaultKind values (a string type), so they compose with browse.FaultPlan
// and the reliability math unchanged.
const (
	// FaultDPIChange — the display DPI changes mid-task, stressing coordinate rescale.
	FaultDPIChange browse.FaultKind = "dpi_change"
	// FaultWindowResize — the focused window resizes mid-task, invalidating the
	// ladder's per-window cache and any stale box.
	FaultWindowResize browse.FaultKind = "window_resize"
	// FaultA11yEmpty — the accessibility tree goes empty (an Electron pane), forcing
	// the Rung 1→2 fallback.
	FaultA11yEmpty browse.FaultKind = "a11y_empty"
	// FaultSparseTreeLies — the tree is non-empty but missing the needed widget,
	// stressing the stagnation-based downgrade trigger.
	FaultSparseTreeLies browse.FaultKind = "sparse_tree_lies"
)

// Faults is the desktop catalog a full reliability sweep iterates — the GUI faults
// above PLUS the transport faults browse already injects (a desktop app can be slow
// or error too).
func Faults() []browse.FaultKind {
	out := []browse.FaultKind{FaultDPIChange, FaultWindowResize, FaultA11yEmpty, FaultSparseTreeLies}
	return append(out, browse.AllFaults()...)
}

// DefaultScenarios is the seed catalog the CI live runner executes against the
// desktop image. Each carries a concrete, machine-checkable success criterion
// (an exact value to extract or text to confirm) so grading is a runnable assertion,
// never a judgment call (the I2 discipline).
func DefaultScenarios() []browse.Scenario {
	return []browse.Scenario{
		{
			Name:        "calculator-add",
			Goal:        "Open the calculator, compute 12 + 30, and record the displayed result as sum.",
			ExpectField: "sum",
			ExpectValue: "42",
		},
		{
			Name:       "text-editor-type",
			Goal:       "Open the text editor and type the phrase 'hello desktop'; confirm it appears.",
			ExpectText: "hello desktop",
		},
		{
			Name:        "settings-toggle",
			Goal:        "Open Settings, navigate to the About page, and record the OS name shown as os_name.",
			ExpectField: "os_name",
			ExpectValue: "Debian",
		},
	}
}
