package main

// repomap.go — the host-side repository-map builder behind the native loop's
// RepoContext seam. Every fresh drive used to start blind (goal-only first turn)
// and spend its first steps on ls/cat rediscovering structure the harness can
// compute in milliseconds. This renders a bounded two-level tree with per-
// directory source-file counts — orientation, not content: the loop's outline/
// read_symbol/codeintel tools remain the way to actually look inside files, and
// the system prompt now points the model at them. Deliberately NO AST parsing
// (the outline tool walks symbols on demand); a map must stay cheap enough to
// compute on every drive, and with the provider-side cache breakpoints it is
// paid for exactly once per run.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// repoMapBudget bounds the rendered map. Small enough to be a rounding error in
// the prompt, large enough for a two-level view of a sizeable repo.
const repoMapBudget = 3 * 1024

// sourceExts is the extension set counted as "source" in the map. Orientation
// only — an unlisted language still shows up via its directories.
var sourceExts = map[string]bool{
	".go": true, ".py": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".rs": true, ".java": true, ".rb": true, ".c": true, ".h": true, ".cc": true,
	".cpp": true, ".hpp": true, ".cs": true, ".kt": true, ".swift": true,
	".md": true, ".sh": true, ".sql": true, ".proto": true, ".yaml": true, ".yml": true,
}

// skipDirs are never descended into or listed: VCS/tool internals and
// dependency trees that would drown the signal.
var skipDirs = map[string]bool{
	".git": true, ".nilcore": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true, ".venv": true, "__pycache__": true,
	"dist": true, "build": true, ".idea": true, ".vscode": true,
}

// repoMap renders the two-level tree of root, bounded to budget bytes. Errors
// degrade to an empty string (the seam treats "" as no map) — orientation must
// never block a run.
func repoMap(root string, budget int) string {
	if budget <= 0 {
		budget = repoMapBudget
	}
	top, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	var rootFiles []string
	for _, e := range top {
		name := e.Name()
		if !e.IsDir() {
			if sourceExts[filepath.Ext(name)] || name == "Makefile" || name == "go.mod" {
				rootFiles = append(rootFiles, name)
			}
			continue
		}
		if skipDirs[name] || strings.HasPrefix(name, ".") {
			continue
		}
		writeDirLine(&sb, root, name)
	}
	if len(rootFiles) > 0 {
		sort.Strings(rootFiles)
		fmt.Fprintf(&sb, "./  %s\n", strings.Join(rootFiles, " "))
	}
	out := sb.String()
	if len(out) > budget {
		cut := strings.LastIndexByte(out[:budget], '\n')
		if cut < 0 {
			cut = budget
		}
		out = out[:cut] + "\n… (map truncated — use outline for detail)"
	}
	return strings.TrimRight(out, "\n")
}

// writeDirLine renders one top-level directory with its source count and its
// second-level children (each with their own counts), one line per top-level
// dir so the map stays scannable.
func writeDirLine(sb *strings.Builder, root, dir string) {
	abs := filepath.Join(root, dir)
	entries, err := os.ReadDir(abs)
	if err != nil {
		return
	}
	var children []string
	direct := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				continue
			}
			if n := countSource(filepath.Join(abs, name)); n > 0 {
				children = append(children, fmt.Sprintf("%s(%d)", name, n))
			} else {
				children = append(children, name)
			}
			continue
		}
		if sourceExts[filepath.Ext(name)] {
			direct++
		}
	}
	line := dir + "/"
	if direct > 0 {
		line += fmt.Sprintf(" %d file(s)", direct)
	}
	if len(children) > 0 {
		sort.Strings(children)
		line += "  → " + strings.Join(children, " ")
	}
	sb.WriteString(line + "\n")
}

// countSource counts source files DIRECTLY under dir (no recursion — the map is
// two levels deep by design; deeper structure is the outline tool's job).
func countSource(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && sourceExts[filepath.Ext(e.Name())] {
			n++
		}
	}
	return n
}
