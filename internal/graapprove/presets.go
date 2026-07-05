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
		// The Deploy $/day CEILING is 5, aligned with the tightest+standard production
		// blast presets (cmd/nilcore/blast.go: tight $1, standard $5) so it is an
		// ATTAINABLE envelope, not dead weight above the runtime fence. MaxDollarsDay is a
		// per-UTC-day CEILING on the ACTUAL auto-approved-dollar total for this class — NOT
		// the cost charged per action. The graded gate charges each action its OWN cost
		// (perActionCost — $0 today, since no GateAction field carries a per-action figure)
		// against the per-day total, and when a blast budget is present routes the same
		// charge through it, so the effective ceiling is min(this ceiling, the blast day
		// budget). Trusted-preset Deploy is therefore reachable under a real
		// `-blast-radius standard` run AND under the DEFAULT `-blast-radius off` (nil meter),
		// where the clause's own MaxDollarsDay bounds the day total in-process. The prior bug
		// charged the run ledger's CUMULATIVE spend (Evidence.SpentUSD) as this action's
		// cost, which — under the default off path — denied every action once the run had
		// spent any money; charging the action's own cost (0 today) fixes reachability.
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
				MaxDollarsDay: 5, // attainable within `-blast-radius standard` ($5/day); see comment above
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
