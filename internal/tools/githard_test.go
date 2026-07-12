package tools

import (
	"strings"
	"testing"
)

// TestHardenArgs pins the clamp flags: the repo-controlled code-execution vectors a
// command-line `-c` can cleanly override — per-repo hooks, the fsmonitor binary, and a
// forced signed commit's gpg.program — must always be neutralized with `-c`, which
// outranks repo-local $GIT_DIR/config. (diff.external is NOT clamped here: `-c
// diff.external=` makes git exec the empty string and breaks every diff; it is covered
// by the .git write-guard + the git tool's per-command --no-ext-diff instead.)
func TestHardenArgs(t *testing.T) {
	got := strings.Join(HardenArgs(), " ")
	want := "-c core.hooksPath=/dev/null -c core.fsmonitor= -c commit.gpgSign=false"
	if got != want {
		t.Fatalf("HardenArgs() = %q, want %q", got, want)
	}
}

// TestHardenedEnvPins asserts the inert config/credential values are appended so
// git ignores system/global config and never blocks on an interactive prompt.
func TestHardenedEnvPins(t *testing.T) {
	env := HardenedEnv()
	for _, want := range []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	} {
		if !contains(env, want) {
			t.Errorf("HardenedEnv() missing pinned value %q", want)
		}
	}
}

// TestHardenedEnvStripsVectors verifies any pre-existing GIT_* config or
// external-behavior vector inherited from the host is dropped, so a poisoned
// ambient environment cannot re-introduce external code execution.
func TestHardenedEnvStripsVectors(t *testing.T) {
	for _, v := range []string{
		"GIT_CONFIG_GLOBAL=/tmp/evil",
		"GIT_CONFIG_NOSYSTEM=0",
		"GIT_TERMINAL_PROMPT=1",
		"GIT_ALLOW_PROTOCOL=ext",
		"GIT_PROXY_COMMAND=/tmp/p",
		"GIT_EXTERNAL_DIFF=/tmp/d",
	} {
		t.Setenv(strings.SplitN(v, "=", 2)[0], strings.SplitN(v, "=", 2)[1])
	}
	env := HardenedEnv()
	// The poisoned ambient values must not survive verbatim.
	for _, bad := range []string{
		"GIT_CONFIG_GLOBAL=/tmp/evil",
		"GIT_CONFIG_NOSYSTEM=0",
		"GIT_TERMINAL_PROMPT=1",
		"GIT_ALLOW_PROTOCOL=ext",
		"GIT_PROXY_COMMAND=/tmp/p",
		"GIT_EXTERNAL_DIFF=/tmp/d",
	} {
		if contains(env, bad) {
			t.Errorf("HardenedEnv() leaked ambient vector %q", bad)
		}
	}
	// And the clamp must still be applied exactly once on top.
	if n := count(env, "GIT_CONFIG_NOSYSTEM=1"); n != 1 {
		t.Errorf("GIT_CONFIG_NOSYSTEM=1 appears %d times, want 1", n)
	}
	if n := count(env, "GIT_CONFIG_GLOBAL=/dev/null"); n != 1 {
		t.Errorf("GIT_CONFIG_GLOBAL=/dev/null appears %d times, want 1", n)
	}
}

func contains(ss []string, want string) bool { return count(ss, want) > 0 }

func count(ss []string, want string) int {
	n := 0
	for _, s := range ss {
		if s == want {
			n++
		}
	}
	return n
}
