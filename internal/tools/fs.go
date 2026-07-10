package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"

	"nilcore/internal/worktreefs"
)

// The symlink-safe path-join, O_NOFOLLOW open, and atomic temp+rename write that
// confine every host-side file op to the worktree now live in the audited leaf
// internal/worktreefs (shared with the artifact store and report writer — auditor
// B1). The thin wrappers below keep this package's existing call sites and tests
// byte-identical while delegating the security-load-bearing logic to that one copy.

// safePath resolves rel against workdir and confirms it stays inside it — both
// lexically AND after following symlinks — so a tool can never read or write
// outside the worktree. Delegates to worktreefs.SafeJoin.
func safePath(workdir, rel string) (string, error) {
	return worktreefs.SafeJoin(workdir, rel)
}

// safeAbs confirms an ABSOLUTE path resolves inside root (symlink-safe). It is the
// read-root counterpart of safePath, used only for READS against the worktree or an
// explicitly-added read root. Delegates to worktreefs.SafeAbs.
func safeAbs(root, abs string) (string, error) {
	return worktreefs.SafeAbs(root, abs)
}

// writeNoFollow writes content to p atomically and without following a symlink at
// the destination, preserving the destination's existing permissions on overwrite
// (default 0644 for a new file). p is an already-confined absolute target (produced
// by safePath); workdir is the worktree root the no-follow parent-dir check is bounded
// to (a symlinked component at/below it is refused, one above it — the host's own
// ancestors — is trusted). We write into p's directory via the confined path so the
// atomic temp+rename + O_NOFOLLOW discipline lives in one place (worktreefs).
func writeNoFollow(workdir, p string, content []byte) error {
	// p is already confined by safePath; WriteConfined performs the atomic temp+rename
	// + O_NOFOLLOW write without re-resolving p (which would fail on a not-yet-existing
	// parent dir). perm 0 ⇒ preserve-existing-else-0644, matching the prior behavior.
	return worktreefs.WriteConfined(workdir, p, content, 0)
}

// readNoFollow reads an already-confined absolute target WITHOUT following a symlink
// at the final component. safePath/safeAbs check the path at CHECK time, but a plain
// os.ReadFile follows a final-component symlink at OPEN time — so a sandboxed process
// racing us can swap the final component for a symlink between the check and the open
// and leak an out-of-worktree file (a TOCTOU escape, I4). Routing every tool read
// through worktreefs.ReadConfined (which opens with O_NOFOLLOW) closes that window:
// a swapped-in link is refused rather than followed. p is confined by the caller.
func readNoFollow(p string) ([]byte, error) {
	return worktreefs.ReadConfined(p)
}

// ReadTool returns the contents of a file in the worktree, or — by absolute path —
// in an added read-only context root. ReadRoots are the extra roots (absolute,
// symlink-resolved at registration); nil ⇒ worktree-only (byte-identical default).
type ReadTool struct {
	ReadRoots []string
}

func (ReadTool) Name() string { return "read" }
func (t ReadTool) Description() string {
	// The paging sentence is how the model LEARNS the recovery move: a truncated
	// result is useless unless the description teaches offset/limit re-reads.
	const paging = " Optional offset/limit select a line window: offset is the 1-based " +
		"first line, limit the max number of lines. Results are byte-capped; a truncated " +
		"result ends with \"[truncated at line N of M total lines — re-read with " +
		"offset=N to continue]\"."
	if len(t.ReadRoots) == 0 {
		return "Read a file in the working directory. Returns its contents." + paging
	}
	return "Read a file by path. Relative paths are in the working directory; added context roots " +
		"are read by absolute path. Returns the file's contents." + paging
}
func (ReadTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"path":{"type":"string"},` +
		`"offset":{"type":"integer","description":"1-based line number to start reading from (default: 1)"},` +
		`"limit":{"type":"integer","description":"maximum number of lines to return (default: to end of file)"}` +
		`},"required":["path"]}`)
}

// maxReadBytes bounds how much of a file one read returns to the model, so a single
// cat-style read of a lockfile or a huge generated file cannot flood the context
// window. web_fetch caps a page at 64KB; reads sit a notch under it because read is
// the highest-frequency tool. The truncation notice names the offset to continue
// from, so nothing becomes unreachable — just paged.
const maxReadBytes = 48 * 1024

func (t ReadTool) Run(_ context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	if in.Offset < 0 {
		return "", fmt.Errorf("bad input: offset must be a 1-based line number (got %d)", in.Offset)
	}
	if in.Limit < 0 {
		return "", fmt.Errorf("bad input: limit must be a line count >= 0 (got %d)", in.Limit)
	}
	p, err := resolveReadable(workdir, t.ReadRoots, in.Path)
	if err != nil {
		return "", err
	}
	// O_NOFOLLOW read: the confinement check in resolveReadable happens before this
	// open, so a plain os.ReadFile would follow a final-component symlink swapped in
	// after the check and leak an out-of-worktree file (I4 TOCTOU). readNoFollow
	// refuses a swapped-in link instead.
	b, err := readNoFollow(p)
	if err != nil {
		return "", err
	}
	// Fast path: a small file with no explicit window returns byte-identical full
	// contents — the overwhelmingly common case behaves exactly as it always has.
	if in.Offset == 0 && in.Limit == 0 && len(b) <= maxReadBytes {
		return string(b), nil
	}
	return boundedRead(string(b), in.Offset, in.Limit), nil
}

// boundedRead returns a line window of src bounded by maxReadBytes, appending a
// harness-authored notice whenever content remains beyond what was returned. offset
// is the 1-based first line (0 ⇒ start of file); limit is a max line count (0 ⇒ to
// EOF). The notice is plain text INSIDE the tool result the loop fences as data — it
// opens/closes no fence itself, so it can never un-fence anything (I7).
func boundedRead(src string, offset, limit int) string {
	lines := strings.Split(src, "\n")
	// A final trailing newline yields one empty trailing element, not a real line.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)
	if offset == 0 {
		offset = 1
	}
	if offset > total {
		return fmt.Sprintf("[no content: offset %d is past end of file — %d total lines]", offset, total)
	}
	window := lines[offset-1:]
	if limit > 0 && limit < len(window) {
		window = window[:limit]
	}

	// Take whole lines while they fit under the byte cap. A single line longer than
	// the cap (e.g. minified output) is clipped and counted as consumed, so paging
	// via the advertised offset still makes progress past it instead of looping.
	var b strings.Builder
	taken, clipped := 0, false
	for _, ln := range window {
		add := len(ln)
		if taken > 0 {
			add++ // the joining newline
		}
		if b.Len()+add > maxReadBytes {
			break
		}
		if taken > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(ln)
		taken++
	}
	if taken == 0 {
		b.WriteString(window[0][:maxReadBytes])
		taken, clipped = 1, true
	}

	next := offset + taken // 1-based first line NOT returned
	switch {
	case taken < len(window) || clipped:
		// The byte cap cut the window short (or clipped an oversized line): the exact
		// wording here is the one the tool description promises the model.
		fmt.Fprintf(&b, "\n[truncated at line %d of %d total lines — re-read with offset=%d to continue]", next, total, next)
	case next <= total:
		// The explicit window was fully delivered, but the file continues past it.
		fmt.Fprintf(&b, "\n[showing lines %d-%d of %d total lines — re-read with offset=%d to continue]", offset, next-1, total, next)
	}
	return b.String()
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
	if err := writeNoFollow(workdir, p, []byte(in.Content)); err != nil {
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
	// O_NOFOLLOW read (see readNoFollow): defends the final component against a
	// symlink swapped in between safePath's check and the open (I4 TOCTOU).
	b, err := readNoFollow(p)
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
	if err := writeNoFollow(workdir, p, []byte(out)); err != nil {
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
		// Never read through a symlink (I4 worktree confinement). filepath.WalkDir
		// yields a symlink as a non-dir entry without descending it — the cached
		// readdir type lets us skip it here. But that type is a snapshot: a
		// sandboxed process racing the walk can swap this entry for a symlink AFTER
		// WalkDir recorded it as a regular file, and a plain os.ReadFile would then
		// FOLLOW the swapped link and leak out-of-worktree content (an I4 TOCTOU).
		// So we both skip the statically-present link here AND read via readNoFollow
		// (O_NOFOLLOW) below — matching the sibling ReadTool's hardening — so a
		// swapped-in final-component link is refused rather than followed.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, d.Name()); !ok {
				return nil
			}
		}
		data, err := readNoFollow(path)
		if err != nil {
			return nil // unreadable file or a swapped-in symlink (O_NOFOLLOW ELOOP): skip
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
