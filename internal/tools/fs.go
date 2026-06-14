package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// safePath resolves rel against workdir and confirms it stays inside it, so a
// tool can never read or write outside the worktree (inspectable confinement).
func safePath(workdir, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	abs := filepath.Join(workdir, rel)
	clean := filepath.Clean(abs)
	root := filepath.Clean(workdir)
	if clean != root && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the worktree", rel)
	}
	return clean, nil
}

// ReadTool returns the contents of a file in the worktree.
type ReadTool struct{}

func (ReadTool) Name() string { return "read" }
func (ReadTool) Description() string {
	return "Read a file in the working directory. Returns its full contents."
}
func (ReadTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
}
func (ReadTool) Run(_ context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Path string `json:"path"`
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
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(in.Content), 0o644); err != nil {
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
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited %s (%d replacement(s))", in.Path, n), nil
}

// SearchTool greps the worktree for a regular expression, optionally limited by a
// filename glob. Returns matching file:line: text lines.
type SearchTool struct{}

func (SearchTool) Name() string { return "search" }
func (SearchTool) Description() string {
	return "Search files for a regular expression (optional filename glob). Returns file:line:text matches."
}
func (SearchTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"glob":{"type":"string"}},"required":["pattern"]}`)
}
func (SearchTool) Run(_ context.Context, workdir string, input json.RawMessage) (string, error) {
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
	walkErr := filepath.WalkDir(workdir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if in.Glob != "" {
			if ok, _ := filepath.Match(in.Glob, d.Name()); !ok {
				return nil
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // unreadable file: skip
		}
		rel, _ := filepath.Rel(workdir, path)
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				fmt.Fprintf(&b, "%s:%d:%s\n", rel, i+1, line)
				count++
				if count >= 500 {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	if count == 0 {
		return "no matches", nil
	}
	return b.String(), nil
}
