package tools

// rename.go — the first WRITE-BACK on the AST seam: a type-scoped symbol rename.
// It attacks the most error-prone structural edit (multi-site rename), which raw
// string edits do by hand and routinely half-finish into a broken build. The Go
// path resolves the binding with go/types (best-effort, tolerant of unresolved
// third-party imports) so it renames the IDENTIFIER THAT BINDS TO THE TARGET OBJECT
// — never a shadowed local or a same-named identifier of a different type. It is
// scoped to the DEFINING package; an exported symbol's uses in other packages are
// reported, not silently rewritten. dry_run defaults to TRUE.
//
// Non-Go is deliberately REFUSED rather than approximated: a heuristic rename that
// silently misses field/type/variable uses is worse than none. Use structural_replace
// or edit for those.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RenameSymbolTool renames a Go package-level symbol or method across its package.
type RenameSymbolTool struct{}

func (RenameSymbolTool) Name() string { return "rename_symbol" }
func (RenameSymbolTool) Description() string {
	return "Type-scoped rename of a Go symbol (func/type/method/var/const) across its package, resolving " +
		"the binding with go/types so shadowed locals are not touched. dry_run defaults true (preview). " +
		"Scoped to the defining package; exported uses in OTHER packages are reported, not rewritten. Go only."
}
func (RenameSymbolTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"old_name":{"type":"string"},"new_name":{"type":"string"},"file":{"type":"string"},"dry_run":{"type":"boolean"}},"required":["old_name","new_name"]}`)
}

func (RenameSymbolTool) Run(ctx context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		OldName string `json:"old_name"`
		NewName string `json:"new_name"`
		File    string `json:"file"`
		DryRun  *bool  `json:"dry_run"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	dryRun := true
	if in.DryRun != nil {
		dryRun = *in.DryRun
	}
	if in.OldName == "" || in.NewName == "" {
		return "", fmt.Errorf("old_name and new_name are required")
	}
	if !token.IsIdentifier(in.NewName) || token.IsKeyword(in.NewName) {
		return "", fmt.Errorf("new_name %q is not a valid Go identifier", in.NewName)
	}
	if in.OldName == in.NewName {
		return "", fmt.Errorf("old_name and new_name are identical")
	}

	// Locate the defining package directory.
	pkgDir, err := renameTargetDir(ctx, workdir, in.OldName, in.File)
	if err != nil {
		return "", err
	}

	fset := token.NewFileSet()
	files, paths, primaryPkg, err := parsePackageDir(fset, pkgDir)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("rename_symbol: no Go source for a package found in %s", relOrSame(workdir, pkgDir))
	}

	info := &types.Info{
		Defs: map[*ast.Ident]types.Object{},
		Uses: map[*ast.Ident]types.Object{},
	}
	conf := types.Config{
		Importer:                 besteffortImporter{base: importer.Default()},
		Error:                    func(error) {}, // best-effort: tolerate unresolved imports
		DisableUnusedImportCheck: true,
	}
	_, _ = conf.Check(primaryPkg, fset, files, info) // error ignored by design

	// Resolve the single target object: a package-level decl or a method named OldName.
	target, err := resolveRenameTarget(info, in.OldName)
	if err != nil {
		return "", err
	}

	// Collect every identifier bound to that object (its definition + all uses).
	rename := map[*ast.Ident]bool{}
	for id, obj := range info.Defs {
		if obj == target {
			rename[id] = true
		}
	}
	for id, obj := range info.Uses {
		if obj == target {
			rename[id] = true
		}
	}
	if len(rename) == 0 {
		return "", fmt.Errorf("rename_symbol: could not bind %q to a renameable object (try passing 'file')", in.OldName)
	}

	// Apply the rename and note which files changed.
	changedFiles := map[*ast.File]int{}
	for i, f := range files {
		_ = i
		n := 0
		ast.Inspect(f, func(node ast.Node) bool {
			if id, ok := node.(*ast.Ident); ok && rename[id] {
				id.Name = in.NewName
				n++
			}
			return true
		})
		if n > 0 {
			changedFiles[f] = n
		}
	}

	// Reprint changed files.
	var changes []renameChange
	for i, f := range files {
		n, ok := changedFiles[f]
		if !ok {
			continue
		}
		var buf bytes.Buffer
		if ferr := format.Node(&buf, fset, f); ferr != nil {
			return "", fmt.Errorf("rename_symbol: reprint %s: %w", relOrSame(workdir, paths[i]), ferr)
		}
		changes = append(changes, renameChange{rel: relOrSame(workdir, paths[i]), count: n, bytes: buf.Bytes()})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].rel < changes[j].rel })

	total := 0
	for _, c := range changes {
		total += c.count
	}

	var sb strings.Builder
	mode := "applied"
	if dryRun {
		mode = "DRY RUN (no files written; pass dry_run=false to apply)"
	}
	fmt.Fprintf(&sb, "rename_symbol %s → %s: %d site(s) across %d file(s) — %s\n", in.OldName, in.NewName, total, len(changes), mode)
	if isExportedName(in.OldName) {
		sb.WriteString("NOTE: exported symbol — uses in OTHER packages are NOT updated here; rename them separately. The verifier will catch any missed reference.\n")
	}
	for _, c := range changes {
		fmt.Fprintf(&sb, "- %s (%d)\n", c.rel, c.count)
	}

	if !dryRun {
		if werr := applyRenameAtomic(workdir, changes); werr != nil {
			return "", werr
		}
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// renameChange is one file's reprinted bytes and the number of sites renamed in it.
type renameChange struct {
	rel   string
	count int
	bytes []byte
}

// applyRenameAtomic writes every changed file, snapshotting prior bytes first and
// restoring them all if any write fails — so a mid-batch failure can never leave a
// half-renamed (broken-build) tree, mirroring patch.go's discipline.
func applyRenameAtomic(workdir string, changes []renameChange) error {
	type snap struct {
		abs  string
		data []byte
	}
	var snaps []snap
	for _, c := range changes {
		p, perr := safePath(workdir, c.rel)
		if perr != nil {
			return perr
		}
		// O_NOFOLLOW read (readNoFollow): snapshot the prior bytes without following a
		// final-component symlink swapped in after safePath's check (I4 TOCTOU) — the
		// same discipline the write-back (writeNoFollow) already uses.
		prior, rerr := readNoFollow(p)
		if rerr != nil {
			return fmt.Errorf("rename_symbol: snapshot %s: %w", c.rel, rerr)
		}
		snaps = append(snaps, snap{abs: p, data: prior})
	}
	for i, c := range changes {
		if werr := writeNoFollow(workdir, snaps[i].abs, c.bytes); werr != nil {
			for j := 0; j < i; j++ { // restore the files already written
				_ = writeNoFollow(workdir, snaps[j].abs, snaps[j].data)
			}
			return fmt.Errorf("rename_symbol: write %s failed, rolled back: %w", c.rel, werr)
		}
	}
	return nil
}

// renameTargetDir resolves the directory of the package that defines old_name. With
// `file` given it is that file's directory; otherwise it is inferred from the
// worktree symbol index, refusing if old_name is defined in more than one package.
func renameTargetDir(ctx context.Context, workdir, old, file string) (string, error) {
	if file != "" {
		// Confinement check (resolves symlinks), but return the UNRESOLVED dir under
		// the worktree so re-derived relative paths round-trip cleanly for write-back.
		if _, err := safePath(workdir, file); err != nil {
			return "", err
		}
		if strings.ToLower(filepath.Ext(file)) != ".go" {
			return "", fmt.Errorf("rename_symbol supports Go only; %s is not a .go file", file)
		}
		return filepath.Dir(filepath.Join(workdir, file)), nil
	}
	syms, _, err := worktreeSymbols(ctx, workdir)
	if err != nil {
		return "", err
	}
	dirs := map[string]bool{}
	for _, s := range syms {
		if s.Name == old {
			dirs[filepath.Dir(filepath.Join(workdir, s.Rel))] = true
		}
	}
	switch len(dirs) {
	case 0:
		return "", fmt.Errorf("rename_symbol: %q not found in any Go package (non-Go is unsupported)", old)
	case 1:
		for d := range dirs {
			return d, nil
		}
	}
	return "", fmt.Errorf("rename_symbol: %q is defined in %d packages — pass 'file' to disambiguate", old, len(dirs))
}

// parsePackageDir parses the primary (non-external-test) Go package in dir: all
// non-test files plus internal (_test.go with the same package name) test files.
// It returns the parsed files, their paths (index-aligned), and the package name.
func parsePackageDir(fset *token.FileSet, dir string) ([]*ast.File, []string, string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, "", err
	}
	type parsed struct {
		file *ast.File
		path string
		pkg  string
		test bool
	}
	var all []parsed
	primary := ""
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".go" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if perr != nil {
			continue // unparseable: skip
		}
		isTest := strings.HasSuffix(e.Name(), "_test.go")
		p := parsed{file: f, path: path, pkg: f.Name.Name, test: isTest}
		all = append(all, p)
		if !isTest && primary == "" {
			primary = f.Name.Name
		}
	}
	if primary == "" {
		// No non-test source; fall back to the most common test package name.
		counts := map[string]int{}
		for _, p := range all {
			counts[p.pkg]++
		}
		best := 0
		for name, c := range counts {
			if c > best {
				best, primary = c, name
			}
		}
	}
	var files []*ast.File
	var paths []string
	for _, p := range all {
		if p.pkg == primary { // include non-test + internal-test files of the package
			files = append(files, p.file)
			paths = append(paths, p.path)
		}
	}
	return files, paths, primary, nil
}

// resolveRenameTarget finds the single package-level-or-method object named old.
// It refuses (rather than guessing) when there are zero or multiple distinct
// objects, so a rename is never applied to the wrong binding.
func resolveRenameTarget(info *types.Info, old string) (types.Object, error) {
	objs := map[types.Object]bool{}
	for id, obj := range info.Defs {
		if obj == nil || id.Name != old {
			continue
		}
		if isPackageLevel(obj) || isMethod(obj) {
			objs[obj] = true
		}
	}
	switch len(objs) {
	case 0:
		return nil, fmt.Errorf("rename_symbol: %q is not a package-level symbol or method (locals are out of scope)", old)
	case 1:
		for o := range objs {
			return o, nil
		}
	}
	return nil, fmt.Errorf("rename_symbol: %q resolves to %d distinct declarations (e.g. methods on different types) — narrow with 'file' or rename a more specific name", old, len(objs))
}

// isPackageLevel reports whether obj is declared at package scope.
func isPackageLevel(obj types.Object) bool {
	p := obj.Parent()
	return p != nil && p.Parent() == types.Universe
}

// isMethod reports whether obj is a method (a func with a receiver).
func isMethod(obj types.Object) bool {
	fn, ok := obj.(*types.Func)
	if !ok {
		return false
	}
	sig, ok := fn.Type().(*types.Signature)
	return ok && sig.Recv() != nil
}

// besteffortImporter wraps the default importer so an unresolvable import yields an
// empty stand-in package instead of failing the whole type-check — letting local
// (in-package) bindings resolve even when third-party deps are not built.
type besteffortImporter struct{ base types.Importer }

func (b besteffortImporter) Import(path string) (*types.Package, error) {
	if b.base != nil {
		if p, err := b.base.Import(path); err == nil {
			return p, nil
		}
	}
	return types.NewPackage(path, ""), nil
}
