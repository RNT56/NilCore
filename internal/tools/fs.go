package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// safePath resolves rel against workdir and confirms it stays inside it — both
// lexically AND after following symlinks — so a tool can never read or write
// outside the worktree. A lexical check alone is not enough: an in-tree symlink
// (e.g. `evil -> /etc`) would otherwise let a write escape. We therefore resolve
// the worktree root and the deepest existing ancestor of the target through
// EvalSymlinks and re-check containment. (Writes additionally go through an
// atomic temp-file + O_NOFOLLOW open + rename to close the TOCTOU window on the
// final component — see writeNoFollow.)
func safePath(workdir, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	root, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root: %w", err)
	}
	root = filepath.Clean(root)
	target := filepath.Clean(filepath.Join(root, rel))
	return confine(root, target, rel)
}

// confine confirms target stays inside root — both lexically AND after following
// symlinks — returning target on success. It is the shared containment check for
// safePath (worktree-relative writes/reads) and safeAbs (absolute reads against an
// added read root): a lexical check alone is not enough because an in-tree symlink
// (e.g. `evil -> /etc`) would let access escape, so we resolve the deepest existing
// ancestor of the target and re-check. ref is the original path, for the error.
func confine(root, target, ref string) (string, error) {
	if !within(root, target) {
		return "", fmt.Errorf("path %q escapes its root", ref)
	}
	// Resolve the deepest existing ancestor (the target itself may not exist yet,
	// e.g. a new file) and confirm it still resolves inside the root — this is what
	// catches an in-tree symlink pointing out.
	probe := target
	for {
		if _, lerr := os.Lstat(probe); lerr == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	real, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", ref, err)
	}
	if !within(root, filepath.Clean(real)) {
		return "", fmt.Errorf("path %q resolves outside its root (symlink escape)", ref)
	}
	return target, nil
}

// safeAbs confirms an ABSOLUTE path resolves inside root (symlink-safe), returning
// the cleaned path. It is the read-root counterpart of safePath: where safePath
// joins a relative path onto the worktree, safeAbs validates a model-supplied
// absolute path against one allowed root. Used only for READS against the worktree
// or an explicitly-added read root — never for a write (writes stay worktree-only).
func safeAbs(root, abs string) (string, error) {
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	return confine(filepath.Clean(rootResolved), filepath.Clean(abs), abs)
}

// within reports whether p is root or lives under it.
func within(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(os.PathSeparator))
}

// writeNoFollow writes content to p atomically and without following a symlink
// at the destination.
//
// Atomicity: we never truncate-in-place. Instead we write the full content into
// a freshly-created temp file in the SAME directory (so os.Rename stays on one
// filesystem and is therefore atomic on POSIX), fsync it so the bytes are durable
// before the rename, then os.Rename it over p. A kill of the harness at any point
// leaves either the old file untouched (rename never happened) or the complete
// new file (rename committed) — never a half-applied, truncated file.
//
// Symlink safety: the temp file is opened with O_CREATE|O_EXCL|O_NOFOLLOW under a
// random, not-yet-existing name, so a symlink swapped in at the temp path cannot
// be followed or clobbered. os.Rename does not follow a symlink at the
// destination — it replaces p (even if p was swapped to a symlink after
// safePath's check) rather than writing through it — so this is at least as
// strong as the previous O_NOFOLLOW-on-final-component TOCTOU defense, and
// safePath already rejects a destination that resolves outside the worktree.
func writeNoFollow(p string, content []byte) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// Preserve the destination's existing permissions on overwrite; default 0644
	// for a new file. Lstat (not Stat) so a symlink at p is not followed here.
	perm := os.FileMode(0o644)
	if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
		perm = fi.Mode().Perm()
	}

	// O_EXCL guarantees a brand-new file under a unique name; O_NOFOLLOW refuses a
	// symlink swapped in at the temp path. os.CreateTemp can't set these flags, so
	// retry on the (vanishingly rare) name collision ourselves.
	var f *os.File
	var tmp string
	for i := 0; ; i++ {
		tmp = filepath.Join(dir, fmt.Sprintf(".nilcore-tmp-%d-%d", os.Getpid(), randUint()))
		var err error
		f, err = os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL|syscall.O_NOFOLLOW, perm)
		if err == nil {
			break
		}
		if errors.Is(err, fs.ErrExist) && i < 1000 {
			continue
		}
		return fmt.Errorf("create temp file: %w", err)
	}

	// From here on, ensure the temp file never lingers if anything fails.
	if _, err := f.Write(content); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// randUint returns a random uint64 for temp-file naming. It is only used to avoid
// name collisions, not for any security property — O_EXCL is what actually
// guarantees a fresh file — so the stdlib math/rand/v2 source is fine.
func randUint() uint64 { return rand.Uint64() }

// ReadTool returns the contents of a file in the worktree, or — by absolute path —
// in an added read-only context root. ReadRoots are the extra roots (absolute,
// symlink-resolved at registration); nil ⇒ worktree-only (byte-identical default).
type ReadTool struct {
	ReadRoots []string
}

func (ReadTool) Name() string { return "read" }
func (t ReadTool) Description() string {
	if len(t.ReadRoots) == 0 {
		return "Read a file in the working directory. Returns its full contents."
	}
	return "Read a file by path. Relative paths are in the working directory; added context roots " +
		"are read by absolute path. Returns the file's full contents."
}
func (ReadTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
}
func (t ReadTool) Run(_ context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	p, err := resolveReadable(workdir, t.ReadRoots, in.Path)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// WriteTool creates or overwrites a file in the worktree.
type WriteTool struct{}

func (WriteTool) Name() string        { return "write" }
func (WriteTool) Description() string { return "Create or overwrite a file in the working directory." }
func (WriteTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`)
}
func (WriteTool) Run(_ context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	p, err := safePath(workdir, in.Path)
	if err != nil {
		return "", err
	}
	if err := writeNoFollow(p, []byte(in.Content)); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), nil
}

// EditTool performs a structured search-and-replace in a file: it replaces the
// exact `old` text with `new`. `old` must appear exactly once unless `all` is set.
type EditTool struct{}

func (EditTool) Name() string { return "edit" }
func (EditTool) Description() string {
	return "Replace an exact substring in a file (structured diff). 'old' must be unique unless 'all' is true."
}
func (EditTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"},"all":{"type":"boolean"}},"required":["path","old","new"]}`)
}
func (EditTool) Run(_ context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Path string `json:"path"`
		Old  string `json:"old"`
		New  string `json:"new"`
		All  bool   `json:"all"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	p, err := safePath(workdir, in.Path)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	src := string(b)
	n := strings.Count(src, in.Old)
	if n == 0 {
		return "", fmt.Errorf("'old' text not found in %s", in.Path)
	}
	if n > 1 && !in.All {
		return "", fmt.Errorf("'old' text appears %d times in %s; set all=true or make it unique", n, in.Path)
	}
	var out string
	if in.All {
		out = strings.ReplaceAll(src, in.Old, in.New)
	} else {
		out = strings.Replace(src, in.Old, in.New, 1)
	}
	if err := writeNoFollow(p, []byte(out)); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited %s (%d replacement(s))", in.Path, n), nil
}

// SearchTool greps the worktree for a regular expression, optionally limited by a
// filename glob, returning matching file:line:text lines. With ReadRoots set it
// also searches each added read-only context root, emitting those matches by
// ABSOLUTE path (so the model can read them back) while worktree matches stay
// worktree-relative. nil ReadRoots ⇒ worktree-only (byte-identical default).
type SearchTool struct {
	ReadRoots []string
}

func (SearchTool) Name() string { return "search" }
func (t SearchTool) Description() string {
	if len(t.ReadRoots) == 0 {
		return "Search files for a regular expression (optional filename glob). Returns file:line:text matches."
	}
	return "Search the working directory and any added context roots for a regular expression " +
		"(optional filename glob). Worktree matches are relative; context-root matches are absolute."
}
func (SearchTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"glob":{"type":"string"}},"required":["pattern"]}`)
}

// searchCap bounds total matches across the worktree and every read root, so a
// pathologically large tree (or many roots) can never flood the model's context.
const searchCap = 500

func (t SearchTool) Run(_ context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Pattern string `json:"pattern"`
		Glob    string `json:"glob"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return "", fmt.Errorf("bad pattern: %w", err)
	}

	var b strings.Builder
	count := 0
	// Worktree first (relative output), then each read root (absolute output). The
	// cap is shared so the total stays bounded regardless of how many roots are added.
	if err := searchRoot(&b, workdir, true, in.Glob, re, &count); err != nil {
		return "", err
	}
	for _, root := range t.ReadRoots {
		if count >= searchCap {
			break
		}
		if err := searchRoot(&b, root, false, in.Glob, re, &count); err != nil {
			return "", err
		}
	}
	if count == 0 {
		return "no matches", nil
	}
	return b.String(), nil
}

// searchRoot walks one root, appending matches to b. When relative is true matches
// are reported worktree-relative (the primary root); otherwise by absolute path (an
// added read root). count is the shared match budget; the walk stops at searchCap.
func searchRoot(b *strings.Builder, root string, relative bool, glob string, re *regexp.Regexp, count *int) error {
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, d.Name()); !ok {
				return nil
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // unreadable file: skip
		}
		label := path
		if relative {
			label, _ = filepath.Rel(root, path)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				fmt.Fprintf(b, "%s:%d:%s\n", label, i+1, line)
				*count++
				if *count >= searchCap {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	return walkErr
}
