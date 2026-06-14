package verify

// Auto-detection lets NilCore point itself at an unfamiliar repo and still find
// the right "done" command without a human spelling it out. The verifier is the
// only authority on done (see verify.go), so picking the verify command must be
// conservative: we only return a real build/test command when a recognizable
// marker is present, and otherwise fall back to a command that always succeeds.
// Detection never decides failure on its own — an unknown layout yields "true",
// not a spurious red, because "we couldn't tell" must not masquerade as "broken".

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Detect inspects a repository directory and returns the best verify command to
// run for it. The order is deliberate: a project's own Makefile verify target is
// the most authoritative signal, then language-ecosystem markers in descending
// specificity. When nothing is recognized it returns a safe no-op ("true") so
// that an undetectable repo never fails by default.
func Detect(dir string) string {
	if hasMakefileVerifyTarget(dir) {
		return "make verify"
	}
	if exists(dir, "go.mod") {
		return "go build ./... && go test ./..."
	}
	if exists(dir, "package.json") {
		return "npm test"
	}
	if exists(dir, "Cargo.toml") {
		return "cargo test"
	}
	if exists(dir, "pyproject.toml") || exists(dir, "setup.py") {
		return "pytest"
	}
	return "true"
}

// DetectOrOverride returns override verbatim when it is non-empty, otherwise it
// falls back to Detect. This is the seam for an operator who knows better than
// auto-detection: an explicit choice always wins.
func DetectOrOverride(dir, override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	return Detect(dir)
}

// exists reports whether name is present directly inside dir.
func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// hasMakefileVerifyTarget reports whether dir contains a Makefile (or makefile)
// declaring a "verify" target. We read the file rather than just checking for a
// Makefile because a Makefile without that target gives us nothing to run; the
// rule must literally begin a line as "verify:" (allowing leading whitespace and
// prerequisites after the colon), which is how make recognizes a target.
func hasMakefileVerifyTarget(dir string) bool {
	for _, name := range []string{"Makefile", "makefile", "GNUmakefile"} {
		path := filepath.Join(dir, name)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		found := scanForVerifyTarget(f)
		_ = f.Close()
		if found {
			return true
		}
	}
	return false
}

// scanForVerifyTarget reports whether any line declares the "verify" target.
func scanForVerifyTarget(r io.Reader) bool {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimLeft(sc.Text(), " \t")
		rest, ok := strings.CutPrefix(line, "verify")
		if !ok {
			continue
		}
		// The target name must be followed immediately by ':' to avoid matching
		// rules like "verify-fast:" or variables like "verifyflag = ...".
		if strings.HasPrefix(rest, ":") {
			return true
		}
	}
	return false
}
