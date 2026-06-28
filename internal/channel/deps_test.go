package channel_test

// deps_test.go — the executable guard for the channel CONTRACT leaf. The channel
// package defines the minimal Receive/Update/Ask transport contract (and the additive
// DraftStreamer/ChoicePoster capabilities). It must never import the orchestrator: a
// channel is wired BY serve, it does not reach into the run machinery. This keeps the
// contract a stable seam and prevents a transport from naming a concrete orchestrator.
// (channel is not stdlib-pure — it legitimately reaches eventlog/policy for the gate +
// audit — so the invariant is direction, not zero-imports.)

import (
	"os/exec"
	"strings"
	"testing"
)

func TestChannelDoesNotImportOrchestrator(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/channel").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/session": "orchestrator (session)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
		"nilcore/internal/swarm":   "orchestrator (swarm)",
	}
	for _, d := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if reason, bad := forbidden[strings.TrimSpace(d)]; bad {
			t.Errorf("the channel contract leaf must not import %q (%s) — it would invert the dependency direction", d, reason)
		}
	}
}
