package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"nilcore/internal/skills"
	"nilcore/internal/tools"
)

// Agent Skills (P5-T01) surface through the SAME tool registry the native loop
// uses, so the frozen core never changes. A SKILL.md file (frontmatter name +
// description, markdown instructions) becomes a `skill_<name>` tool that returns
// its instructions on demand — unused skills cost ~zero context, exactly like the
// MCP-as-code pattern. Discovery happens at the cmd layer (not inside
// tools.Default) because skills imports tools, so loading there would cycle.

var (
	skillToolsOnce sync.Once
	skillToolsMemo []tools.Tool
)

// skillsDir resolves where Agent Skills are discovered: $NILCORE_SKILLS_DIR if set,
// else <user-config-dir>/nilcore/skills. The feature is opt-in — an absent
// directory simply yields no skills.
func skillsDir() string {
	if d := os.Getenv("NILCORE_SKILLS_DIR"); d != "" {
		return d
	}
	if cfg, err := os.UserConfigDir(); err == nil {
		return filepath.Join(cfg, "nilcore", "skills")
	}
	return ""
}

// loadSkillTools discovers every SKILL.md under dir and exposes them as loop tools.
// Best-effort and pure (no memoization, for testing): an empty/absent directory or
// an unparseable skill yields a warning, never a failure — skills are additive.
func loadSkillTools(dir string) []tools.Tool {
	if dir == "" {
		return nil
	}
	if _, err := os.Stat(dir); err != nil {
		return nil // no skills directory configured/present
	}
	loaded, err := skills.LoadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nilcore: skipping skills in %s: %v\n", dir, err)
		return nil
	}
	return skills.New(loaded).AsTools()
}

// skillTools discovers skills once per process and memoizes the result, so the
// per-task (and per-subagent) registry build never re-walks the directory.
func skillTools() []tools.Tool {
	skillToolsOnce.Do(func() {
		skillToolsMemo = loadSkillTools(skillsDir())
		if n := len(skillToolsMemo); n > 0 {
			fmt.Fprintf(os.Stderr, "nilcore: loaded %d skill(s) from %s\n", n, skillsDir())
		}
	})
	return skillToolsMemo
}

// loopTools is the native loop's tool registry: the structured built-ins plus any
// discovered Agent Skills, in one registry. It replaces tools.Default() at the
// primary backend construction sites (run/chat/serve/build) so every surface that
// drives the loop sees installed skills. (Read-only subagent roles keep the bare
// tools.Default set — skills are a primary-loop capability.)
func loopTools() *tools.Registry {
	r := tools.Default()
	// The read-only code-intelligence tool (graph-native callers/callees/repomap,
	// plus the compiler-grade LSP "precise" lens when NILCORE_LSP_COMMAND is set)
	// belongs on every primary loop, not only the build understander — "understand
	// before you change" applies to run/chat/serve too.
	r.Register(tools.CodeintelTool{})
	// Host-side structural tools (worktree-confined, deterministic, stdlib-only, no
	// execution — I4/I6). They sharpen the inner loop: precise navigation (outline,
	// read_symbol), hygiene/architecture lenses (dead_code, import_graph), safer
	// edits (edit_checked, format_file, patch), and durable working memory (plan).
	for _, t := range hostStructuralTools() {
		r.Register(t)
	}
	for _, t := range skillTools() {
		r.Register(t)
	}
	return r
}

// hostStructuralTools is the set of additional host-side structured tools the
// primary loop registers (run/chat/serve/build). Listed in one place so the
// surface is auditable. The read-only members are also exposed to the Discuss/Plan
// modes via tools.ReadOnlyWithCodeintel.
func hostStructuralTools() []tools.Tool {
	return []tools.Tool{
		tools.OutlineTool{},
		tools.ReadSymbolTool{},
		tools.DeadCodeTool{},
		tools.ImportGraphTool{},
		tools.FormatTool{},
		tools.EditCheckedTool{},
		tools.PatchTool{},
		tools.PlanTool{},
		tools.AffectedTestsTool{},
		tools.RenameSymbolTool{},
		tools.StructuralReplaceTool{},
	}
}
