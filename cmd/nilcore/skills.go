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
	for _, t := range skillTools() {
		r.Register(t)
	}
	return r
}
