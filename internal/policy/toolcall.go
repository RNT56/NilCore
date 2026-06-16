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

// ReadOnlyCommandPolicy tightens the default command denylist for a read-only
// drive: on top of the destructive/host-boundary defaults it denies the obvious
// in-tree write vectors (redirection, tee, in-place sed, mv/cp), unattended git
// writes (commit/add), and package installs. It is the command-plane mirror of a
// write-free registry — defense in depth for any read-only drive that still has a
// shell, so a `run` that reaches for a file write is denied at the guard.
//
// IMPORTANT: a substring denylist is best-effort, NOT a robust write boundary on
// its own (a determined model can write via a vector not on the list). A drive
// that must GUARANTEE no writes (the conversational Plan/Discuss modes) therefore
// removes the shell entirely (backend.Native.DisableShell) and relies on the
// write-free registry; this policy is the secondary belt, not the only one. It was
// lifted from internal/roster so the chat front door and the roles share one list.
func ReadOnlyCommandPolicy() CommandPolicy {
	denied := append([]string{}, DefaultCommandPolicy().Denied...)
	denied = append(denied,
		" > ",        // output redirection into the tree
		" >>",        // append redirection
		">>",         // append redirection (no leading space)
		"tee ",       // tee into a file
		"sed -i",     // in-place edit
		"perl -i",    // in-place edit
		"mv ",        // move into/over tree files
		"cp ",        // copy into the tree
		"truncate ",  // truncate a file
		"install ",   // install a file
		"git commit", // unattended git write
		"git add",    // staging is a write step toward commit
		"git apply",  // apply a patch into the tree
		"git checkout",
		"git reset",
		"git clean",
		"pip install",
		"npm install",
		"npm i ",
		"go install",
		"apt-get install",
		"apt install",
		"yum install",
		"brew install",
		"cargo install",
		"gem install",
	)
	return CommandPolicy{Denied: denied}
}

func strconvQuote(s string) string { return "\"" + s + "\"" }
