package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func productionFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no production files found")
	}
	return fset, files
}

// REQ-004: the domain depends only on the standard library.
func TestDomainImportsOnlyStandardLibrary(t *testing.T) {
	_, files := productionFiles(t)
	for _, f := range files {
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") || first == "wagering" {
				t.Errorf("%s imports non-stdlib package %s", f.Name.Name, path)
			}
		}
	}
}

// REQ-009: no floating point anywhere in production code.
func TestNoFloatingPoint(t *testing.T) {
	fset, files := productionFiles(t)
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name == "float32" || x.Name == "float64" {
					t.Errorf("%s: floating-point type %s", fset.Position(x.Pos()), x.Name)
				}
			case *ast.BasicLit:
				if x.Kind == token.FLOAT {
					t.Errorf("%s: floating-point literal %s", fset.Position(x.Pos()), x.Value)
				}
			case *ast.SelectorExpr:
				if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "strconv" && (x.Sel.Name == "ParseFloat" || x.Sel.Name == "FormatFloat") {
					t.Errorf("%s: strconv.%s", fset.Position(x.Pos()), x.Sel.Name)
				}
			}
			return true
		})
	}
}
