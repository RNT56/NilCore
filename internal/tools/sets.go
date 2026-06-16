package tools

// sets.go holds the canonical capability tool SETS — the named registries the
// loop's callers (the multi-agent roles in internal/roster AND the conversational
// front door's Discuss/Plan modes in cmd/nilcore) hand to backend.Native. They
// live here, beside Default(), so "what tools does a read-only drive get" is one
// shared definition rather than a per-caller copy (the read-only roles in roster
// were the original home; the chat modes are the second caller).
//
// The read-only sets are the STRUCTURAL half of the no-write guarantee (I7,
// capability via wiring): they carry NO write/edit/git tool, so a drive handed one
// has no registered path to mutate the tree — the guarantee is a property of the
// registry, never of the model choosing to behave. (The shell `run` tool is the
// loop's own always-on fallback; a read-only drive additionally suppresses it via
// backend.Native.DisableShell, so even the shell escape is closed structurally.)

// ReadOnly is the minimal read-only structured set: read + search ONLY. The
// GitTool is deliberately excluded even though it offers status/diff/log, because
// the same tool also does add/commit (a git-write surface) — read-only means no
// registered path to mutate the tree. It mirrors what internal/roster handed its
// read-only roles (researcher/planner/reviewer) before this set was lifted here to
// be shared with the chat front door.
func ReadOnly() *Registry {
	return NewRegistry(ReadTool{}, SearchTool{})
}

// ReadOnlyWithCodeintel is the read-only set for a code-UNDERSTANDING drive: the
// read/search pair PLUS the codeintel tool. CodeintelTool is a host-side,
// read-only adapter (it parses the worktree into an ephemeral in-memory graph and
// returns a context bundle — no write, no execution, no network), so adding it
// keeps the write-free structural guarantee intact. It is the right set for
// Discuss/Plan, which must navigate an unfamiliar codebase to converse or plan.
func ReadOnlyWithCodeintel() *Registry {
	return NewRegistry(ReadTool{}, SearchTool{}, CodeintelTool{})
}
