package tools

// format.go — deterministic, host-side canonicalization with zero sandbox cost.
// gofmt-clean is an explicit project standard (CLAUDE.md §4), and discovering a
// formatting slip only after a full sandboxed `make verify` container round-trip is
// a wasted loop. format_file runs go/format.Source (Go) or json re-indent (JSON)
// in-process and writes the canonical bytes. It is format-only — NOT goimports (that
// needs the banned golang.org/x/tools), so it never rewrites imports — and it
// fails SOFT on unparseable input (returns the error, writes nothing), so it can
// never corrupt a file or masquerade as the verifier (I2).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/format"
	"path/filepath"
	"strings"
)

// FormatTool canonicalizes a single file's formatting.
type FormatTool struct{}

func (FormatTool) Name() string { return "format_file" }
func (FormatTool) Description() string {
	return "Canonicalize a file's formatting in place: gofmt for .go, 2-space re-indent for .json. " +
		"With check=true it only reports whether the file is unformatted (no write). Fails soft on " +
		"unparseable input (writes nothing). Not goimports — never rewrites imports. No execution."
}
func (FormatTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"check":{"type":"boolean"}},"required":["path"]}`)
}

func (FormatTool) Run(_ context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Path  string `json:"path"`
		Check bool   `json:"check"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	p, err := safePath(workdir, in.Path)
	if err != nil {
		return "", err
	}
	// O_NOFOLLOW read (readNoFollow): safePath checks the path at CHECK time, but a
	// plain os.ReadFile follows a final-component symlink swapped in after the check
	// and would leak an out-of-worktree file (I4 TOCTOU). readNoFollow refuses a
	// swapped-in link instead.
	src, err := readNoFollow(p)
	if err != nil {
		return "", err
	}

	ext := strings.ToLower(filepath.Ext(in.Path))
	var canonical []byte
	switch ext {
	case ".go":
		canonical, err = format.Source(src)
		if err != nil {
			// Unparseable Go: report and write nothing (fail soft).
			return fmt.Sprintf("format_file %s: not gofmt-clean — file does not parse: %v", in.Path, err), nil
		}
	case ".json":
		var buf bytes.Buffer
		if err := json.Indent(&buf, src, "", "  "); err != nil {
			return fmt.Sprintf("format_file %s: not valid JSON: %v", in.Path, err), nil
		}
		canonical = buf.Bytes()
	default:
		return fmt.Sprintf("format_file %s: no formatter for %q (only .go and .json) — left unchanged", in.Path, ext), nil
	}

	if bytes.Equal(canonical, src) {
		return fmt.Sprintf("format_file %s: already formatted", in.Path), nil
	}
	if in.Check {
		return fmt.Sprintf("format_file %s: NOT formatted (%d → %d bytes); run without check to apply", in.Path, len(src), len(canonical)), nil
	}
	if err := writeNoFollow(workdir, p, canonical); err != nil {
		return "", err
	}
	return fmt.Sprintf("format_file %s: reformatted (%d → %d bytes)", in.Path, len(src), len(canonical)), nil
}
