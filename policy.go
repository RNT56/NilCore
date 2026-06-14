// Package policy decides what the agent may do unattended. The rule chosen for
// nullclaw: auto-run reversible actions, pause for a human gate on irreversible
// ones. Inside a worktree + container almost everything is reversible, so gates
// naturally concentrate at the integration boundary (merge, push, deploy, pay).
package policy

import "strings"

type Class int

const (
	Reversible Class = iota
	Irreversible
)

func (c Class) String() string {
	if c == Irreversible {
		return "irreversible"
	}
	return "reversible"
}

// irreversibleSignals are matched against a normalized action description. The
// list is intentionally conservative: when unsure, treat it as irreversible.
var irreversibleSignals = []string{
	"git push", "push --force", "force-push",
	"merge", "rebase --onto", "git reset --hard",
	"deploy", "kubectl apply", "terraform apply",
	"rm -rf /", "drop table", "delete from",
	"npm publish", "docker push",
	"curl", "wget", // network egress
	"pay", "charge", "transfer",
}

// Classify labels an action by reversibility + blast radius.
func Classify(action string) Class {
	a := strings.ToLower(action)
	for _, sig := range irreversibleSignals {
		if strings.Contains(a, sig) {
			return Irreversible
		}
	}
	return Reversible
}

// Approver asks a human to approve an irreversible action. Phase 0 can use a
// console approver; the Telegram channel supplies an interactive one in Phase 1,
// where the gate simply becomes a chat reply.
type Approver interface {
	Approve(action string) bool
}

// Gate reports whether the action may proceed right now.
func Gate(action string, ask Approver) bool {
	if Classify(action) == Reversible {
		return true
	}
	if ask == nil {
		return false // no approver wired => deny irreversible by default
	}
	return ask.Approve(action)
}
