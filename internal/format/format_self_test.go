package format_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/vearutop/gocan/internal/format"
)

func TestFormatDoesNotDropDecls(t *testing.T) {
	src, err := os.ReadFile("format.go")
	if err != nil {
		t.Fatalf("read format.go: %v", err)
	}

	cfg := format.DefaultConfig()
	out, err := format.FormatFile("format.go", src, cfg)
	if err != nil {
		t.Fatalf("format: %v", err)
	}

	before, err := declKeysFromSrc(src)
	if err != nil {
		t.Fatalf("parse before: %v", err)
	}
	after, err := declKeysFromSrc(out)
	if err != nil {
		t.Fatalf("parse after: %v", err)
	}

	missing := diffKeys(before, after)
	if len(missing) > 0 {
		t.Fatalf("formatter dropped decls: %s", strings.Join(missing, ", "))
	}
}

func declKeysFromSrc(src []byte) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "src.go", src, 0)
	if err != nil {
		return nil, err
	}

	var keys []string
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					keys = append(keys, "type:"+s.Name.Name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						keys = append(keys, d.Tok.String()+":"+n.Name)
					}
				}
			}
		case *ast.FuncDecl:
			if d.Recv == nil {
				keys = append(keys, "func:"+d.Name.Name)
				continue
			}
			recv := recvTypeName(d.Recv.List[0].Type)
			keys = append(keys, "method:"+recv+"."+d.Name.Name)
		}
	}

	sort.Strings(keys)
	return keys, nil
}

func recvTypeName(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		if id, ok := v.X.(*ast.Ident); ok {
			return "*" + id.Name
		}
	}
	return "<unknown>"
}

func diffKeys(a, b []string) []string {
	set := make(map[string]struct{}, len(b))
	for _, k := range b {
		set[k] = struct{}{}
	}
	var missing []string
	for _, k := range a {
		if _, ok := set[k]; !ok {
			missing = append(missing, k)
		}
	}
	return missing
}
