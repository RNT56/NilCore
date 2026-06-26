package autosrc_test

// deps_test.go — the executable LEAF guard (I6, the trust/blastbudget pattern).
//
// autosrc is a LEAF: it unifies self-start sources into one bounded priority queue
// and hands each admitted goal to an INJECTED handler. The handler is injected
// precisely so this package never imports the orchestrator — the wiring layer
// installs autosrc and supplies the verified/gated drivegate path, never the
// reverse. Importing agent/super/project would invert that direction and let "which
// source fires next" reach back into "what is done", eroding the I2 boundary. It may
// import the trigger/eventlog leaves (+ their transitive store/policy) and stdlib —
// nothing in the orchestrator tier and no external module. This test walks the full
// transitive import set via `go list -deps` and fails the build on any forbidden
// import. It lives in the external test package so it inspects the real dependency
// closure from outside.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestAutosrcLeafDependencyClosure(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/autosrc").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	// Forbidden ORCHESTRATOR packages: a leaf importing any of these inverts the
	// dependency direction (the orchestrator wires the daemon, never the reverse).
	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
	}

	// Sanctioned external-module prefixes (CLAUDE.md §2 I6): autosrc is NOT a pure
	// leaf — it imports the trigger/eventlog leaves, which transitively reach the
	// SQLite store. So the SQLite driver and golang.org/x/sys legitimately enter the
	// closure exactly as they do for the trust leaf. They are the project's THREE
	// sanctioned exceptions; any OTHER external module is a new dependency and must
	// fail (autosrc adds zero modules of its own — I6).
	sanctioned := []string{
		"golang.org/x/sys",           // x/sys (sandbox syscalls; transitive via SQLite)
		"modernc.org/",               // the pure-Go SQLite driver + its support modules
		"github.com/google/uuid",     // pulled by modernc.org/sqlite
		"github.com/mattn/go-isatty", // pulled by modernc.org/libc
		"github.com/dustin/go-humanize",
		"github.com/ncruces/go-strftime",
		"github.com/remyoudompheng/bigfft",
	}
	for _, d := range deps {
		d = strings.TrimSpace(d)
		if reason, bad := forbidden[d]; bad {
			t.Errorf("autosrc leaf imports forbidden package %q (%s)", d, reason)
		}
		// A non-stdlib module path carries a dotted domain in its first segment (e.g.
		// "golang.org/x/sys"); stdlib and nilcore paths never do. Such a path must be
		// one of the sanctioned exceptions, else autosrc has added a new module (I6).
		if first := strings.SplitN(d, "/", 2)[0]; strings.Contains(first, ".") && first != "nilcore" {
			if !hasAnyPrefix(d, sanctioned) {
				t.Errorf("autosrc leaf must add no NEW external module, found unsanctioned %q", d)
			}
		}
	}

	// Positive assertion: the sanctioned reads are in the closure, so a refactor that
	// silently drops one (e.g. stops feeding trigger.Signal through) is visible here.
	wantPresent := []string{
		"nilcore/internal/trigger",
		"nilcore/internal/eventlog",
	}
	have := map[string]bool{}
	for _, d := range deps {
		have[strings.TrimSpace(d)] = true
	}
	for _, w := range wantPresent {
		if !have[w] {
			t.Errorf("autosrc leaf is expected to import %q but it is absent from the closure", w)
		}
	}
}

// hasAnyPrefix reports whether s starts with any prefix in prefixes.
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
