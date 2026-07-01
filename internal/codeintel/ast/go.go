// Go-language backend. This is a behavior-identical move of the original
// std-library (go/parser) implementation behind the languageParser seam — the same
// AST walk, the same Kind mapping, the same spans, the same error propagation. Keep
// it byte-for-byte equivalent to what callers saw before the seam existed.
package ast

import (
	goast "go/ast"
	"go/parser"
	"go/token"
)

// goParser parses Go source via the standard library. It is stateless; the FileSet
// is per-call so parsing one file never depends on another.
type goParser struct{}

func (goParser) symbols(path string) ([]Symbol, error) {
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
				kind, recv = KindMethod, goRecvType(d.Recv.List[0].Type)
			}
			syms = append(syms, Symbol{Name: d.Name.Name, Kind: kind, Recv: recv, Span: goSpan(fset, path, d.Pos(), d.End())})
		case *goast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *goast.TypeSpec:
					syms = append(syms, Symbol{Name: s.Name.Name, Kind: KindType, Span: goSpan(fset, path, s.Pos(), s.End())})
				case *goast.ValueSpec:
					kind := KindVar
					if d.Tok == token.CONST {
						kind = KindConst
					}
					for _, n := range s.Names {
						syms = append(syms, Symbol{Name: n.Name, Kind: kind, Span: goSpan(fset, path, n.Pos(), n.End())})
					}
				}
			}
		}
	}
	return syms, nil
}

func (goParser) references(path string) ([]Reference, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var refs []Reference
	add := func(id *goast.Ident) {
		refs = append(refs, Reference{Name: id.Name, Span: goSpan(fset, path, id.Pos(), id.End())})
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

func (goParser) calls(path string) (map[string][]string, error) {
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
		// Append rather than assign: two FuncDecls with the same bare name (e.g. a
		// method on two receivers, or a method shadowing a free function) hash to
		// the same key, and a plain assignment would let the second silently drop
		// the first's callees. Appending merges them so no outgoing call is lost;
		// the graph dedups identical edges via its UNIQUE(from_id,to_id,kind).
		out[fd.Name.Name] = append(out[fd.Name.Name], called...)
	}
	return out, nil
}

func goRecvType(expr goast.Expr) string {
	switch t := expr.(type) {
	case *goast.StarExpr:
		return goRecvType(t.X)
	case *goast.Ident:
		return t.Name
	}
	return ""
}

func goSpan(fset *token.FileSet, path string, start, end token.Pos) Span {
	return Span{File: path, StartLine: fset.Position(start).Line, EndLine: fset.Position(end).Line}
}
