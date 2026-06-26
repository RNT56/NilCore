package tools

// editchecked.go — the SWE-agent "ACI" robustness lesson at NilCore's tightest
// feedback radius: an edit that parses its own result and REJECTS the change if it
// newly breaks syntax, returning the parse error + a source window so the model
// fixes it in the same turn instead of discovering an opaque failure several steps
// later in `make verify`. Same surface as `edit` plus an optional gofmt-on-accept.
//
// Two disciplines that keep it from becoming an anti-feature:
//   - newly-broken-ONLY: if the file already failed to parse, the edit is allowed
//     (you must be able to fix a broken file).
//   - Go-precise, others-passthrough: only .go gets the go/parser gate; every other
//     language behaves exactly like `edit` (no false rejections from a half-baked
//     non-Go validator). gofmt-on-accept uses go/format only (no import rewriting).

import (
	"context"
	"encoding/json"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// EditCheckedTool is `edit` with a syntax gate for Go.
type EditCheckedTool struct{}

func (EditCheckedTool) Name() string { return "edit_checked" }
func (EditCheckedTool) Description() string {
	return "Like edit (replace exact 'old' with 'new'; 'old' unique unless all=true) but, for .go files, " +
		"parses the result and REJECTS the edit if it newly breaks syntax — returning the error and the " +
		"offending lines so you can fix it now. Optional gofmt=true reformats on accept. Non-Go behaves like edit."
}
func (EditCheckedTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"},"all":{"type":"boolean"},"gofmt":{"type":"boolean"}},"required":["path","old","new"]}`)
}

func (EditCheckedTool) Run(_ context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Path  string `json:"path"`
		Old   string `json:"old"`
		New   string `json:"new"`
		All   bool   `json:"all"`
		Gofmt bool   `json:"gofmt"`
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
	var candidate string
	if in.All {
		candidate = strings.ReplaceAll(src, in.Old, in.New)
	} else {
		candidate = strings.Replace(src, in.Old, in.New, 1)
	}

	note := ""
	if strings.ToLower(filepath.Ext(in.Path)) == ".go" {
		origErr := parseGo(in.Path, src)
		candErr := parseGo(in.Path, candidate)
		// Reject ONLY when the edit newly breaks a previously-parsing file.
		if origErr == nil && candErr != nil {
			return "", fmt.Errorf("edit_checked %s REJECTED — the change breaks Go syntax (file left unchanged):\n%s",
				in.Path, syntaxWindow(candidate, candErr))
		}
		if origErr == nil && candErr == nil {
			note = " [parse ok]"
			if in.Gofmt {
				if formatted, ferr := format.Source([]byte(candidate)); ferr == nil {
					candidate = string(formatted)
					note = " [parse ok, gofmt'd]"
				}
			}
		} else {
			note = " [file already had parse errors — not gated]"
		}
	}

	if err := writeNoFollow(p, []byte(candidate)); err != nil {
		return "", err
	}
	return fmt.Sprintf("edit_checked %s (%d replacement(s))%s", in.Path, n, note), nil
}

// parseGo parses src as a Go file, returning the first error (or nil). The filename
// only labels positions in errors.
func parseGo(name, src string) error {
	_, err := parser.ParseFile(token.NewFileSet(), name, src, parser.SkipObjectResolution)
	return err
}

// syntaxWindow renders the first parse error plus a few lines of context around it,
// so the model sees exactly where the candidate broke. It extracts the line number
// from the scanner error text (the "file:line:col:" prefix) and falls back to just
// the message when it cannot.
func syntaxWindow(src string, perr error) string {
	msg := perr.Error()
	line := firstErrorLine(msg)
	if line <= 0 {
		return msg
	}
	lines := strings.Split(src, "\n")
	lo := line - 3
	if lo < 1 {
		lo = 1
	}
	hi := line + 3
	if hi > len(lines) {
		hi = len(lines)
	}
	var sb strings.Builder
	sb.WriteString(msg)
	sb.WriteString("\n")
	for i := lo; i <= hi; i++ {
		marker := "  "
		if i == line {
			marker = "→ "
		}
		fmt.Fprintf(&sb, "%s%d: %s\n", marker, i, lines[i-1])
	}
	return strings.TrimRight(sb.String(), "\n")
}

// firstErrorLine pulls the line number out of a "name:line:col: message" scanner
// error string. Returns 0 when no position is present.
func firstErrorLine(msg string) int {
	// Find the first "...:<line>:<col>:" — split on ':' and look for two consecutive
	// numeric fields.
	parts := strings.Split(msg, ":")
	for i := 0; i+1 < len(parts); i++ {
		if a, err := atoiTrim(parts[i]); err == nil && a > 0 {
			if _, err2 := atoiTrim(parts[i+1]); err2 == nil {
				return a
			}
		}
	}
	return 0
}

func atoiTrim(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
