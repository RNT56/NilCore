package main

import (
	"os"
	"strings"

	"nilcore/internal/sandbox"
	"nilcore/internal/verify"
)

// behavioralVerifier builds the project verifier, optionally composed with a
// headless-browser behavioral check (P9-T03). When NILCORE_BROWSER_VERIFY is set
// (to the in-sandbox browser-driver command that navigates the running app and
// exits non-zero on a broken render), the verdict ANDs the project's own checks
// with a verify.BrowserVerifier — so a change that builds and tests green but
// renders broken still ships RED. The verifier stays the sole authority on "done"
// (I2); the browser result is an INPUT to the verdict, never a self-report. Unset
// ⇒ exactly verify.New (byte-identical). It is applied to whole-app drives (run /
// chat / serve / resume), not to individual build subagents — a behavioral check
// belongs at the app level, not per-component.
func behavioralVerifier(box sandbox.Sandbox, cmd string) verify.Verifier {
	base := verify.New(box, cmd)
	bcmd := strings.TrimSpace(os.Getenv("NILCORE_BROWSER_VERIFY"))
	if bcmd == "" {
		return base
	}
	return verify.Composite{Named: []verify.NamedVerifier{
		{Name: "checks", V: base},
		{Name: "browser", V: verify.NewBrowser(box, bcmd)},
	}}
}
