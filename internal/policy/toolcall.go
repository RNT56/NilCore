package policy

import "strings"

// CommandPolicy validates a shell command before it runs in the sandbox (P2-T04).
// It is a hard denylist of obviously-destructive or boundary-crossing patterns —
// an extra layer on top of the sandbox and the egress allowlist. A denied call
// is reported back to the model as a structured error instead of executing.
// Empty policy allows everything (the pre-P2 behavior).
type CommandPolicy struct {
	Denied []string // case-insensitive substrings that deny a command
}

// Check reports whether cmd may run, and if not, why. The command and each
// pattern are whitespace-normalized first (see collapseWS), so padding like
// "rm  -rf  /" or "git\tpush" cannot evade the denylist (audit L4).
func (p CommandPolicy) Check(cmd string) (allowed bool, reason string) {
	lc := collapseWS(strings.ToLower(cmd))
	for _, d := range p.Denied {
		if strings.Contains(lc, collapseWS(strings.ToLower(d))) {
			return false, "blocked by tool-call policy: matches denied pattern " + strconvQuote(d)
		}
	}
	return true, ""
}

// DefaultCommandPolicy denies clearly-destructive and host-boundary commands.
// Network exfiltration is already handled by the egress allowlist; this catches
// filesystem destruction, privilege/host changes, and unattended irreversible git.
func DefaultCommandPolicy() CommandPolicy {
	return CommandPolicy{Denied: []string{
		"rm -rf /",
		"rm -rf /*",
		":(){",      // fork bomb
		"mkfs",      // format a filesystem
		"dd if=",    // raw disk write
		"> /dev/sd", // overwrite a block device
		"chmod -R 777 /",
		"shutdown",
		"reboot",
		"git push", // irreversible: must go through the gate, not the loop
		"git remote add",
		"sudo ",
	}}
}

func strconvQuote(s string) string { return "\"" + s + "\"" }
