package graapprove

import (
	"testing"
	"time"

	"nilcore/internal/blastbudget"
	"nilcore/internal/policy"
)

// mintProdBlast mirrors cmd/nilcore/blast.go's "standard" production blast preset
// ($5/day auto-approval dollars) so this test proves reachability against a REAL
// production configuration, not a synthetic one. Kept in-package to avoid importing
// the cmd wiring; the numbers are pinned by TestTrustedDeployReachableConfigsMatchDocs
// against the documented values.
func mintProdBlast(dollarsDay float64) *blastbudget.Budget {
	b := blastbudget.New()
	b.SetIrreversibleCeiling(5)
	b.SetAutoApprovalDollarCeiling(dollarsDay)
	return b
}

// TestTrustedPresetDeployReachable is the regression for the headline finding: the
// `trusted` preset's Deploy clause must be AUTO-APPROVABLE under at least one real
// production config. Before the actual-spend fix the gate charged the WHOLE clause
// ceiling ($25) per action, so every production blast preset (tight $1 / standard $5)
// refused it (over_ceiling), and with no blast meter it denied dollar_ceiling_unmetered
// — trusted-preset Deploy was unreachable in EVERY production config. Now the gate
// charges the action's ACTUAL incremental cost, so an earned deploy within the day
// budget auto-approves.
func TestTrustedPresetDeployReachable(t *testing.T) {
	env, err := Preset("trusted")
	if err != nil {
		t.Fatalf("Preset(trusted): %v", err)
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("trusted preset must validate: %v", err)
	}

	// Locate the deploy clause and earn its (high) trust bar on staging.
	var deploy ClassClause
	var found bool
	for _, c := range env.Classes {
		if c.Type == "deploy" {
			deploy, found = c, true
		}
	}
	if !found {
		t.Fatal("trusted preset must contain a deploy clause")
	}

	dir := t.TempDir()
	now := time.Now().UTC() // real day so the greens count as recent under RecencyDays
	// Earn MinSample greens on staging (MinSuccesses == MinSample for the preset).
	path := writeLog(t, dir, greenRun("deploy", "staging", deploy.MinSample+2))

	cases := []struct {
		name  string
		blast *blastbudget.Budget
		// action cost: a small actual spend that fits the day budget, plus the $0 case.
		spentUSD float64
	}{
		{"standard blast, $2 actual cost", mintProdBlast(5), 2},
		{"standard blast, $0 actual cost", mintProdBlast(5), 0},
		{"no blast meter, $0 actual cost", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			human := &recHuman{reply: false} // must NOT be consulted on a real auto-approval
			sink := &recSink{}
			g := newGraded(human, env, path, tc.blast,
				WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

			act := policy.GateAction{Type: policy.Deploy, Branch: "staging"}
			if tc.spentUSD > 0 {
				act.Evidence = &policy.GateEvidence{SpentUSD: tc.spentUSD}
			}
			if !g.ApproveStructured(act) {
				ev, _ := sink.last()
				t.Fatalf("trusted-preset deploy must auto-approve under %s (last event: %+v)", tc.name, ev.detail)
			}
			if human.called {
				t.Fatalf("a reachable auto-approval must not consult the human (%s)", tc.name)
			}
			ev, ok := sink.last()
			if !ok || ev.kind != "auto_approve" {
				t.Fatalf("expected auto_approve, got %+v", sink.events)
			}
		})
	}
}

// TestTrustedDeployCeilingIsAttainable pins that the trusted preset's Deploy $/day
// ceiling does not sit ABOVE the documented production blast budgets — a ceiling that
// exceeds every blast preset is effectively dead weight (the smaller blast fence always
// wins via min). The standard blast preset is $5/day (cmd/nilcore/blast.go), so the
// clause ceiling must be <= 5 to be an attainable envelope on its own terms.
func TestTrustedDeployCeilingIsAttainable(t *testing.T) {
	env, err := Preset("trusted")
	if err != nil {
		t.Fatalf("Preset(trusted): %v", err)
	}
	const standardBlastDollarsDay = 5.0 // cmd/nilcore/blast.go blastPresets["standard"].dollarsDay
	for _, c := range env.Classes {
		if c.Type != "deploy" {
			continue
		}
		if c.MaxDollarsDay > standardBlastDollarsDay {
			t.Fatalf("trusted deploy MaxDollarsDay=%v exceeds the standard blast day budget %v — unattainable envelope",
				c.MaxDollarsDay, standardBlastDollarsDay)
		}
		if c.MaxDollarsDay <= 0 {
			t.Fatalf("trusted deploy MaxDollarsDay=%v must be a positive ceiling", c.MaxDollarsDay)
		}
	}
}
