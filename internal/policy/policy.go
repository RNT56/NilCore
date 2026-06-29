// Package policy decides what the agent may do unattended. The rule chosen for
// nilcore: auto-run reversible actions, pause for a human gate on irreversible
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
	a := collapseWS(strings.ToLower(action))
	for _, sig := range irreversibleSignals {
		if containsWord(a, collapseWS(sig)) {
			return Irreversible
		}
	}
	return Reversible
}

// containsWord reports whether needle occurs in haystack on WORD BOUNDARIES, so a
// bare signal like "merge"/"pay"/"curl" matches the command word but NOT a larger
// word that merely contains it ("merger", "display", "payload", "recharge",
// "curly", "transferable"). A boundary is only required at an end whose needle char
// is itself a word char, so multi-word/punctuated signals ("git push", "rm -rf /",
// "git reset --hard") still match as phrases. Both inputs are already lowercased +
// whitespace-collapsed by the caller.
func containsWord(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	for start := 0; ; {
		i := strings.Index(haystack[start:], needle)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(needle)
		leftOK := i == 0 || !isWordByte(needle[0]) || !isWordByte(haystack[i-1])
		rightOK := end == len(haystack) || !isWordByte(needle[len(needle)-1]) || !isWordByte(haystack[end])
		if leftOK && rightOK {
			return true
		}
		start = i + 1
	}
}

// isWordByte reports whether b is an ASCII word character (letter, digit, underscore).
// The action is already lowercased, so only lowercase letters appear.
func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// collapseWS collapses every run of whitespace (spaces, tabs, newlines) to a
// single space and trims, so a denied or irreversible pattern cannot be slipped
// past a substring check with padding like "rm  -rf" or "git\tpush" (audit L4).
func collapseWS(s string) string { return strings.Join(strings.Fields(s), " ") }

// Approver asks a human to approve an irreversible action. Phase 0 can use a
// console approver; the Telegram channel supplies an interactive one in Phase 1,
// where the gate simply becomes a chat reply.
type Approver interface {
	Approve(action string) bool
}

// Gate reports whether the action may proceed right now. It is the FREE-TEXT gate
// primitive (it Classify-s an arbitrary command/action string for reversibility); the
// structured integration-boundary path uses GateStructured/GateAction instead. Retained
// as the public primitive paired with Classify for gating a model-emitted command string
// — not dead despite the typed path being the live orchestrator seam.
func Gate(action string, ask Approver) bool {
	if Classify(action) == Reversible {
		return true
	}
	if ask == nil {
		return false // no approver wired => deny irreversible by default
	}
	return ask.Approve(action)
}
