// Package ast is the structural foundation of code intelligence (P3-T09): parse
// source to symbols (functions, types, methods, vars, consts) and references (the
// "tag map"), each carrying a source span. Parsing is per-file, so re-parsing a
// single changed file is inherently incremental.
//
// Scope note: this implementation is Go-first, built on the standard library
// (go/parser) — no cgo. The Symbol/Reference types and the per-file API are the
// stable seam; a tree-sitter backend for other languages slots in behind it later
// without changing callers (kept out now to preserve the zero-cgo build).
package ast

import (
	goast "go/ast"
	"go/parser"
	"go/token"
)

// Kind classifies a symbol.
type Kind string

const (
	KindFunc   Kind = "func"
	KindMethod Kind = "method"
	KindType   Kind = "type"
	KindVar    Kind = "var"
	KindConst  Kind = "const"
)

// Span is a source location (1-based line range).
type Span struct {
	File      string
	StartLine int
	EndLine   int
}

// Symbol is a declared name.
type Symbol struct {
	Name string
	Kind Kind
	Recv string // receiver type, for methods
	Span Span
}

// Reference is a use of a name (call or selection).
type Reference struct {
	Name string
	Span Span
}

// Symbols extracts the declared symbols from a Go source file.
func Symbols(path string) ([]Symbol, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var syms []Symbol
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *goast.FuncDecl:
			kind, recv := KindFunc, ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind, recv = KindMethod, recvType(d.Recv.List[0].Type)
			}
			syms = append(syms, Symbol{Name: d.Name.Name, Kind: kind, Recv: recv, Span: span(fset, path, d.Pos(), d.End())})
		case *goast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *goast.TypeSpec:
					syms = append(syms, Symbol{Name: s.Name.Name, Kind: KindType, Span: span(fset, path, s.Pos(), s.End())})
				case *goast.ValueSpec:
					kind := KindVar
					if d.Tok == token.CONST {
						kind = KindConst
					}
					for _, n := range s.Names {
						syms = append(syms, Symbol{Name: n.Name, Kind: kind, Span: span(fset, path, n.Pos(), n.End())})
					}
				}
			}
		}
	}
	return syms, nil
}

// References extracts called/selected names (the reference edges) from a Go file.
func References(path string) ([]Reference, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var refs []Reference
	add := func(id *goast.Ident) {
		refs = append(refs, Reference{Name: id.Name, Span: span(fset, path, id.Pos(), id.End())})
	}
	goast.Inspect(f, func(n goast.Node) bool {
		switch e := n.(type) {
		case *goast.CallExpr:
			switch fn := e.Fun.(type) {
			case *goast.Ident:
				add(fn)
			case *goast.SelectorExpr:
				add(fn.Sel)
			}
		case *goast.SelectorExpr:
			add(e.Sel)
		}
		return true
	})
	return refs, nil
}

// Calls returns, per top-level function/method, the names it calls — the raw
// material for the call graph (P3-T10).
func Calls(path string) (map[string][]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, decl := range f.Decls {
		fd, ok := decl.(*goast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		var called []string
		goast.Inspect(fd.Body, func(n goast.Node) bool {
			if ce, ok := n.(*goast.CallExpr); ok {
				switch fn := ce.Fun.(type) {
				case *goast.Ident:
					called = append(called, fn.Name)
				case *goast.SelectorExpr:
					called = append(called, fn.Sel.Name)
				}
			}
			return true
		})
		out[fd.Name.Name] = called
	}
	return out, nil
}

func recvType(expr goast.Expr) string {
	switch t := expr.(type) {
	case *goast.StarExpr:
		return recvType(t.X)
	case *goast.Ident:
		return t.Name
	}
	return ""
}

func span(fset *token.FileSet, path string, start, end token.Pos) Span {
	return Span{File: path, StartLine: fset.Position(start).Line, EndLine: fset.Position(end).Line}
}
