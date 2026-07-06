package graapprove

import "fmt"

// commonDeny is the floor every preset enforces: the protected branches that NO
// preset may ever admit. main/master/release/* are denied structurally; deny
// always wins over an allowlist (see graded.go scope checks). prod is denied for
// Deploy by a dedicated structural rule plus an explicit prod* deny here.
var commonDeny = []string{"main", "master", "release/*", "release", "prod", "prod*"}

// Preset returns a conservative/standard/trusted starter envelope by name. An
// unknown or blank name returns a zero Envelope and an error — the feature is off
// unless a recognized preset (or an explicit envelope) is configured. By
// construction NO preset's clause admits main/master/release/prod for any class
// (asserted by a test): the operator opts into auto-approval on safe scopes only.
func Preset(name string) (Envelope, error) {
	switch name {
	case "conservative":
		// OpenPR only — opening a draft PR is the most reversible boundary action
		// (close the draft; no merge ever happens).
		return Envelope{Classes: []ClassClause{
			{
				Type:          "open-pr",
				AllowBranches: []string{"*"},
				DenyBranches:  commonDeny,
				MinSuccesses:  5,
				MinSample:     5,
				RecencyDays:   14,
				MaxPerDay:     3,
				MaxDollarsDay: 0,
			},
		}}, nil
	case "standard":
		// + PromoteToBase on non-main branches (reset/delete the non-main branch to
		// undo). main/master/release stay denied.
		return Envelope{Classes: []ClassClause{
			{
				Type:          "open-pr",
				AllowBranches: []string{"*"},
				DenyBranches:  commonDeny,
				MinSuccesses:  10,
				MinSample:     10,
				RecencyDays:   14,
				MaxPerDay:     2,
				MaxDollarsDay: 0,
			},
			{
				Type:          "promote-to-base",
				AllowBranches: []string{"*"},
				DenyBranches:  commonDeny,
				MinSuccesses:  10,
				MinSample:     10,
				RecencyDays:   14,
				MaxPerDay:     2,
				MaxDollarsDay: 0,
			},
			{
				// Binding a model-authored acceptance verifier is a trust escalation
				// (the agent judging its own work), so it carries a HIGHER earned-trust
				// bar than open-pr: a self-check auto-approves only after that exact
				// check has proven itself many times. Scope (carried as Branch) is the
				// verifier id bound to a hash of its command (id@<cmd-hash>), so a
				// CHANGED command re-gates; the bound check can only ADD to the bar (I2).
				Type:          "bind-self-authored",
				AllowBranches: []string{"*"},
				DenyBranches:  commonDeny,
				MinSuccesses:  15,
				MinSample:     15,
				RecencyDays:   7,
				MaxPerDay:     3,
				MaxDollarsDay: 0,
			},
		}}, nil
	case "trusted":
		// + Deploy to staging (redeploy previous to undo), bounded by a $/day cap.
		// prod* is always denied — both via Environments allowlisting staging only
		// and the structural prod* deny in the graded scope check.
		//
		// DORMANT until a deploy flow exists (docs/ROADMAP-DEPLOY.md). This deploy clause is
		// tested scaffolding, NOT a currently-reachable path: no production code constructs a
		// policy.GateAction{Type: Deploy}, so the graded gate's Deploy branch never fires in a
		// real run today (a `-blast-radius standard` run does not create one — the only gated
		// action the swarm/build paths emit is PromoteToBase). It is kept so the earned-trust +
		// Environments-allowlist mechanism is ready the moment the roadmapped deploy flow lands
		// and starts constructing Deploy GateActions; the graded.go Deploy branch carries the
		// matching note.
		//
		// When that flow arrives, MaxDollarsDay is a per-UTC-day CEILING on the ACTUAL
		// auto-approved-dollar total for this class — NOT the cost charged per action. The
		// graded gate charges each action its OWN cost (perActionCost — $0 today, since no
		// GateAction field carries a per-action figure) against the per-day total, and when a
		// blast budget is present routes the same charge through it, so the effective ceiling is
		// min(this ceiling, the blast day budget). The $/day CEILING of 5 is aligned with the
		// standard production blast preset (cmd/nilcore/blast.go: tight $1, standard $5) so the
		// envelope is ATTAINABLE — not dead weight above the runtime fence — once deploy actions
		// flow through it.
		return Envelope{Classes: []ClassClause{
			{
				Type:          "open-pr",
				AllowBranches: []string{"*"},
				DenyBranches:  commonDeny,
				MinSuccesses:  20,
				MinSample:     20,
				RecencyDays:   7,
				MaxPerDay:     2,
				MaxDollarsDay: 0,
			},
			{
				Type:          "promote-to-base",
				AllowBranches: []string{"*"},
				DenyBranches:  commonDeny,
				MinSuccesses:  20,
				MinSample:     20,
				RecencyDays:   7,
				MaxPerDay:     2,
				MaxDollarsDay: 0,
			},
			{
				Type:          "deploy",
				AllowBranches: []string{"*"},
				DenyBranches:  commonDeny,
				Environments:  []string{"staging"},
				MinSuccesses:  20,
				MinSample:     20,
				RecencyDays:   7,
				MaxPerDay:     2,
				MaxDollarsDay: 5, // dormant until ROADMAP-DEPLOY deploy flow; then attainable within `-blast-radius standard` ($5/day)
			},
			{
				// Self-acceptance binding, trusted tier: a higher bar than `standard`.
				// Scope (Branch) is the verifier id bound to a hash of its command
				// (id@<cmd-hash>) — a changed command re-gates; can only ADD to the bar (I2).
				Type:          "bind-self-authored",
				AllowBranches: []string{"*"},
				DenyBranches:  commonDeny,
				MinSuccesses:  25,
				MinSample:     25,
				RecencyDays:   7,
				MaxPerDay:     3,
				MaxDollarsDay: 0,
			},
		}}, nil
	default:
		return Envelope{}, fmt.Errorf("graapprove: unknown preset %q (want conservative|standard|trusted)", name)
	}
}
