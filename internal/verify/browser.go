package verify

import (
	"context"
	"strings"

	"nilcore/internal/sandbox"
)

// BrowserVerifier runs a headless-browser behavioral check INSIDE the sandbox and
// reports pass/fail from the driver command's exit code — exactly like
// CommandVerifier, but named for its role: it is the verifier-side counterpart to
// the browser_view tool. The browser result is evidence the verifier consumes
// (invariant I2), never a model self-report. Because it runs through the same
// sandbox box as the build (the worktree's container), it inherits I4 confinement
// and the egress allowlist.
//
// Command is the browser-driver invocation (e.g. a headless Chromium that
// navigates the dev URL and exits non-zero if a required selector is missing or
// the console logged an error). The browser binary must be present in the sandbox
// image (P0-T03); when it is absent the command exits non-zero and the check is
// red — fail-closed, never a false green.
type BrowserVerifier struct {
	Box     sandbox.Sandbox
	Command string
}

// NewBrowser returns a BrowserVerifier bound to a worktree's sandbox box.
func NewBrowser(box sandbox.Sandbox, command string) *BrowserVerifier {
	return &BrowserVerifier{Box: box, Command: command}
}

func (v *BrowserVerifier) Check(ctx context.Context) (Report, error) {
	if v.Box == nil || strings.TrimSpace(v.Command) == "" {
		// Misconfigured: fail closed rather than silently pass.
		return Report{Passed: false, Output: "browser verifier: no sandbox or command configured"}, nil
	}
	res, err := v.Box.Exec(ctx, v.Command)
	if err != nil {
		return Report{}, err
	}
	out := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
	return Report{Passed: res.ExitCode == 0, Output: tail(out, 4000)}, nil
}
