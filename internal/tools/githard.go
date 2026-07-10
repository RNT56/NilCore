package tools

import (
	"os"
	"strings"
)

// Git-hardening clamp, shared so every host-side git invocation (the `git` tool
// here, and worktree/integration git in other packages) is neutralized the same
// way. A model can write into a worktree — including .git/hooks and .git/config —
// so any host-side `git` we run must not let repo-controlled config or a planted
// hook/fsmonitor binary execute on the host. The defense has two halves that must
// always travel together: per-invocation `-c` flags (HardenArgs) and an
// environment clamp (HardenedEnv).

// HardenArgs returns the `-c` flags to prefix to every git invocation. They
// neutralize the repo-controlled code-execution vectors that survive the environment
// clamp and that a command-line `-c` CAN cleanly override (it outranks repo-local
// $GIT_DIR/config): per-repo hooks, the fsmonitor hook binary, and a forced-signed
// commit's gpg.program (`commit.gpgSign=false` stops a repo-local
// `commit.gpgSign=true`+`gpg.program=…` from invoking an attacker program on `commit`).
//
// We deliberately do NOT clamp `diff.external` here: `-c diff.external=` sets the
// external-diff program to the EMPTY string, which git then tries to EXECUTE on any
// non-empty diff ("cannot run : No such file or directory" → exit 128), breaking every
// `git diff`. No `-c` value disables it (the correct switch, --no-ext-diff, is
// per-command, not global). `diff.external`, the `filter.<name>.clean/smudge` drivers,
// and named `diff.<name>.command` drivers are all covered instead by the PRIMARY
// defense — refusing file writes inside `.git` (worktreefs.writeNoFollow) — so a
// driver's definition can never be planted in repo-local .git/config to begin with;
// the git tool additionally passes --no-ext-diff on its own diff/show ops (git.go).
// These `-c` flags are defense-in-depth on top of the write-guard.
func HardenArgs() []string {
	return []string{
		"-c", "core.hooksPath=/dev/null", // disable all repo hooks (pre-commit, etc.)
		"-c", "core.fsmonitor=", // disable any fsmonitor hook binary
		"-c", "commit.gpgSign=false", // never invoke gpg.program via a forced signed commit
	}
}

// HardenedEnv returns the process environment with every external-config and
// credential-prompt vector stripped, then pinned to inert values: git ignores
// /etc/gitconfig and ~/.gitconfig (which could carry [core] hooksPath, aliases,
// or credential helpers) and never blocks on an interactive prompt. Combined with
// the per-invocation `-c` flags from HardenArgs, a repo a model can write to
// cannot make git run arbitrary code on the host.
func HardenedEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src)+4)
	for _, e := range src {
		switch {
		case strings.HasPrefix(e, "GIT_CONFIG"),
			strings.HasPrefix(e, "GIT_TERMINAL_PROMPT="),
			strings.HasPrefix(e, "GIT_ALLOW_PROTOCOL="),
			strings.HasPrefix(e, "GIT_PROXY_COMMAND="),
			strings.HasPrefix(e, "GIT_EXTERNAL_DIFF="):
			continue // drop anything that could re-introduce external behavior
		}
		out = append(out, e)
	}
	return append(out,
		"GIT_CONFIG_NOSYSTEM=1",       // ignore /etc/gitconfig
		"GIT_CONFIG_GLOBAL=/dev/null", // ignore ~/.gitconfig
		"GIT_TERMINAL_PROMPT=0",       // never prompt for credentials
	)
}
