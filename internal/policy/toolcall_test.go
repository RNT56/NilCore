package policy

import (
	"strings"
	"testing"
)

func TestCommandPolicy(t *testing.T) {
	p := DefaultCommandPolicy()
	allowed := []string{
		"go test ./...",
		"ls -la",
		"git status",
		"git commit -m 'wip'",
		"sed -i 's/a/b/' main.go",
	}
	denied := []string{
		"rm -rf /",
		"echo x && rm -rf /*",
		"mkfs.ext4 /dev/sda1",
		"dd if=/dev/zero of=/dev/sda",
		"git push origin main",
		"sudo apt-get install x",
		":(){ :|:& };:",
	}
	for _, c := range allowed {
		if ok, reason := p.Check(c); !ok {
			t.Errorf("Check(%q) denied (%s), want allowed", c, reason)
		}
	}
	for _, c := range denied {
		if ok, _ := p.Check(c); ok {
			t.Errorf("Check(%q) allowed, want denied", c)
		}
	}
}

func TestCommandPolicyEmptyAllowsAll(t *testing.T) {
	var p CommandPolicy
	if ok, _ := p.Check("rm -rf /"); !ok {
		t.Error("empty policy should allow everything")
	}
}

func TestCommandPolicyReasonNoSecret(t *testing.T) {
	p := DefaultCommandPolicy()
	_, reason := p.Check("git push origin main")
	if !strings.Contains(reason, "git push") {
		t.Errorf("reason should name the pattern: %q", reason)
	}
}

// TestCommandPolicyWhitespaceEvasion proves padding (extra spaces, tabs,
// newlines) cannot slip a destructive command past the denylist (audit L4).
func TestCommandPolicyWhitespaceEvasion(t *testing.T) {
	p := DefaultCommandPolicy()
	evasions := []string{
		"rm  -rf  /",        // doubled spaces
		"rm\t-rf /",         // tab
		"rm -rf\t/",         // tab before root
		"echo hi\nrm -rf /", // newline-spliced
		"git   push origin", // padded git push
		"git\tpush",         // tab git push
		"sudo\t apt-get install x",
	}
	for _, c := range evasions {
		if ok, _ := p.Check(c); ok {
			t.Errorf("Check(%q) allowed, want denied (whitespace evasion)", c)
		}
	}
	// Benign commands with ordinary spacing remain allowed.
	for _, c := range []string{"go test ./...", "git status", "rm -rf ./build/tmp"} {
		if ok, reason := p.Check(c); !ok {
			t.Errorf("Check(%q) denied (%s), want allowed", c, reason)
		}
	}
}
