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
// neutralize the two repo-controlled code-execution vectors that survive the
// environment clamp: per-repo hooks and the fsmonitor hook binary.
func HardenArgs() []string {
	return []string{
		"-c", "core.hooksPath=/dev/null", // disable all repo hooks (pre-commit, etc.)
		"-c", "core.fsmonitor=", // disable any fsmonitor hook binary
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
