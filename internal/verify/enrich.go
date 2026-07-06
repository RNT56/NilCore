package verify

// enrich.go — additive, pure enrichment helpers for the verify EVENT, not the
// verify RUN. Nothing here changes how a Verifier checks, what a Report carries,
// or whether work ships: these are read-only derivations a LATER task folds into
// a verify event's Detail as {verifier_id, fail_class, content_hash, toolchain}
// (docs/ROADMAP-CLOSED-LOOP.md §4 Pillar 3, LRN). With no caller, the verify
// package behaves byte-for-byte as before — these symbols are simply unreferenced.
//
// Three disciplines bind this file:
//
//   - I7 (untrusted input is data, never instructions). The fail-class is a label
//     from a FIXED structural vocabulary — it is derived from the SHAPE of a failed
//     check (its name prefix, the failing command's first token), NEVER from the
//     raw output bytes. Report.Output is consulted only to read the structural
//     "check <name> failed:" envelope the composite verifier writes; no free-form
//     output ever becomes part of the returned label.
//   - I4 (worktree confinement). The content hash walks a worktree through
//     worktreefs (SafeAbs + O_NOFOLLOW), so an in-tree symlink can never make the
//     walk read outside root.
//   - Determinism. Same input ⇒ same hash, across runs and platforms; a changed
//     file ⇒ a changed hash. The toolchain string is likewise a pure function of
//     the running Go runtime, with no host- or secret-derived input (I3).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"nilcore/internal/worktreefs"
)

// Structural fail-class vocabulary. A derived label is ALWAYS one of these
// constants — never raw verifier output. New checks map onto the closest existing
// class so the learned signal stays low-cardinality and leak-free (I7).
const (
	FailClassBuild   = "build"   // compilation / `go build` / `cargo build` style
	FailClassTest    = "test"    // `go test` / `pytest` / `npm test` style
	FailClassLint    = "lint"    // `go vet` / `golangci-lint` / formatter style
	FailClassBrowser = "browser" // the behavioral browser check (composite)
	FailClassPass    = ""        // a passing report has no fail-class
	FailClassUnknown = "other"   // a real failure we could not classify structurally
)

// failClassPrefixes maps a structural token (a check name or a command's first
// token, lower-cased) to its fail-class. Order of preference is handled by the
// caller scanning the recognized envelope; this map is the single vocabulary.
var failClassTokens = map[string]string{
	"build":         FailClassBuild,
	"go-build":      FailClassBuild,
	"compile":       FailClassBuild,
	"cargo":         FailClassBuild,
	"tsc":           FailClassBuild,
	"test":          FailClassTest,
	"go-test":       FailClassTest,
	"pytest":        FailClassTest,
	"jest":          FailClassTest,
	"npm":           FailClassTest,
	"vet":           FailClassLint,
	"go-vet":        FailClassLint,
	"lint":          FailClassLint,
	"golangci-lint": FailClassLint,
	"gofmt":         FailClassLint,
	"goimports":     FailClassLint,
	"fmt":           FailClassLint,
	"clippy":        FailClassLint,
	"eslint":        FailClassLint,
	"browser":       FailClassBrowser,
}

// FailClass returns a STRUCTURAL label for a Report's failure, drawn from the
// fixed vocabulary above — never raw output (I7). It is a no-op signal for a pass
// (returns FailClassPass, the empty string).
//
// Derivation, in order, using ONLY structural shape:
//
//  1. A passing report ⇒ FailClassPass.
//  2. The composite verifier prefixes a failure with "check <name> failed:". If
//     that envelope is present, <name> is matched against the vocabulary — this is
//     a label the harness itself assigns to each named check, not model/tool text.
//  3. Otherwise the FIRST whitespace-delimited token of the first output line is
//     treated as the failing command's program name and matched against the
//     vocabulary. The token is a single shell word (e.g. "go", "golangci-lint"),
//     and "go" is disambiguated by its subcommand ("go test" ⇒ test, "go vet" ⇒
//     lint, "go build" ⇒ build) — again, structure only.
//  4. An unrecognized real failure ⇒ FailClassUnknown ("other"). We never echo the
//     unrecognized bytes; an unknown shape collapses to a single safe label.
func FailClass(r Report) string {
	if r.Passed {
		return FailClassPass
	}
	out := r.Output

	// (2) The composite envelope: "check <name> failed:\n...". Read only the name.
	if name, ok := compositeCheckName(out); ok {
		if cls := classifyToken(name, ""); cls != "" {
			return cls
		}
		// A named check we don't recognize is still a structural fact, but its name is
		// harness-assigned (not raw output), so fall through to command sniffing on the
		// inner body below rather than leaking the name.
		if _, body, found := strings.Cut(out, "\n"); found {
			out = body
		}
	}

	// (3) The failing command's program name = the first token of the first line.
	prog, arg := firstCommandTokens(out)
	if prog == "" {
		return FailClassUnknown
	}
	if cls := classifyToken(prog, arg); cls != "" {
		return cls
	}
	// (4) A real failure we cannot place structurally.
	return FailClassUnknown
}

// compositeCheckName extracts the <name> from a "check <name> failed:" envelope
// (the exact prefix Composite.Check writes). It returns ("", false) when the
// envelope is absent, so FailClass falls through to command sniffing. The name is
// a single token; anything longer is treated as not-an-envelope to avoid pulling
// arbitrary text out of a malformed line.
func compositeCheckName(out string) (string, bool) {
	line := out
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		line = out[:i]
	}
	rest, ok := strings.CutPrefix(line, "check ")
	if !ok {
		return "", false
	}
	name, ok := strings.CutSuffix(rest, " failed:")
	if !ok {
		return "", false
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, " \t") {
		return "", false
	}
	return name, true
}

// firstCommandTokens returns the first and second whitespace tokens of the first
// non-empty line of out — the failing command's program name and (for "go") its
// subcommand. Both are lower-cased. This reads SHAPE (which program), not content.
func firstCommandTokens(out string) (prog, arg string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		prog = strings.ToLower(fields[0])
		if len(fields) > 1 {
			arg = strings.ToLower(fields[1])
		}
		return prog, arg
	}
	return "", ""
}

// classifyToken maps a program token (+ optional first arg, used only to split the
// "go" multitool) to a fail-class, or "" when unrecognized. The arg is consulted
// ONLY for the structural "go <subcommand>" split — never folded into the label.
func classifyToken(prog, arg string) string {
	prog = strings.ToLower(strings.TrimSpace(prog))
	if prog == "go" {
		switch strings.ToLower(strings.TrimSpace(arg)) {
		case "test":
			return FailClassTest
		case "vet":
			return FailClassLint
		case "build", "install", "run":
			return FailClassBuild
		default:
			// `go fmt`, `go generate`, an unknown subcommand — build is the safe default
			// for the toolchain's compile-adjacent multitool.
			return FailClassBuild
		}
	}
	// Strip a path so "/usr/bin/golangci-lint" matches "golangci-lint".
	if i := strings.LastIndexByte(prog, '/'); i >= 0 {
		prog = prog[i+1:]
	}
	return failClassTokens[prog]
}

// Toolchain returns a deterministic toolchain-version string identifying the Go
// runtime that produced a verdict: "<go-version>/<goos>/<goarch>" (e.g.
// "go1.22.0/darwin/arm64"). It is a pure function of the running runtime with NO
// host- or secret-derived input (I3), so it is stable for a given toolchain and
// changes when the toolchain is bumped — exactly the invalidation key vcache wants
// folded in. A later task may concatenate project-pinned tool digests onto this;
// this base is the runtime identity.
func Toolchain() string {
	return runtime.Version() + "/" + runtime.GOOS + "/" + runtime.GOARCH
}

// ContentHashWorktree returns a deterministic content hash over the source tree
// rooted at root, excluding version-control and scratch metadata by default
// (.git, .nilcore) plus any extra skipDirs. The same tree always yields the same
// hash; any change to a tracked file, mode bit, or symlink target changes it.
//
// It is the worktree counterpart of ContentHashFiles and shares the same canonical
// folding: entries are walked, confined through worktreefs.SafeAbs, sorted by
// relative path, and folded with a length-prefixed, type-tagged line per entry so
// no two distinct trees can alias to one digest. Files are read with O_NOFOLLOW so
// an in-tree symlink cannot redirect the read outside root (I4).
func ContentHashWorktree(ctx context.Context, root string, skipDirs ...string) (string, error) {
	skip := map[string]bool{".git": true, ".nilcore": true}
	for _, d := range skipDirs {
		if d != "" {
			skip[d] = true
		}
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root: %w", err)
	}

	var entries []hashEntry
	walkErr := filepath.WalkDir(resolvedRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rel, rerr := filepath.Rel(resolvedRoot, path)
		if rerr != nil {
			return fmt.Errorf("relativize %q: %w", path, rerr)
		}
		if rel == "." {
			return nil
		}
		top := rel
		if i := strings.IndexByte(rel, filepath.Separator); i >= 0 {
			top = rel[:i]
		}
		if skip[top] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if _, serr := worktreefs.SafeAbs(resolvedRoot, path); serr != nil {
			return fmt.Errorf("confine %q: %w", rel, serr)
		}
		line, lerr := entryLine(rel, path, d)
		if lerr != nil {
			return lerr
		}
		if line != "" {
			entries = append(entries, hashEntry{rel: rel, line: line})
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("hashing worktree: %w", walkErr)
	}
	return foldEntries(entries), nil
}

// hashEntry is one folded entry, sorted by rel for a walk-order-independent fold.
type hashEntry struct {
	rel  string
	line string
}

// foldEntries produces the canonical hex SHA-256 over a set of entries: sort by
// rel, then fold each line length-prefixed and NUL-terminated so two different
// entry sets cannot collide by concatenation aliasing. An empty set has a stable,
// well-defined digest (the SHA-256 of no input).
func foldEntries(entries []hashEntry) string {
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%d:", len(e.line))
		_, _ = io.WriteString(h, e.line)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// entryLine builds the canonical, deterministic line for one entry. A directory
// and a symlink fold structural identity only (a symlink also folds its target so
// re-pointing a link changes the hash); a regular file folds its mode and a
// content digest; anything else folds a type tag so its mere presence still
// affects the hash without us trying to read it.
func entryLine(rel, path string, d fs.DirEntry) (string, error) {
	switch {
	case d.IsDir():
		return "dir\x00" + rel, nil
	case d.Type()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return "", fmt.Errorf("readlink %q: %w", rel, err)
		}
		return "link\x00" + rel + "\x00" + target, nil
	case d.Type().IsRegular():
		digest, mode, err := fileContentDigest(path)
		if err != nil {
			return "", fmt.Errorf("digest %q: %w", rel, err)
		}
		return fmt.Sprintf("file\x00%s\x00%o\x00%s", rel, mode, digest), nil
	default:
		return "other\x00" + rel + "\x00" + d.Type().String(), nil
	}
}

// fileContentDigest returns the hex SHA-256 of a regular file's bytes plus its
// permission bits, reading it with O_NOFOLLOW so a symlink swapped in at the final
// component is refused rather than followed (I4).
func fileContentDigest(path string) (digest string, mode fs.FileMode, err error) {
	f, err := worktreefs.OpenNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), fi.Mode().Perm(), nil
}
