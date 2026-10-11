package money

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoFloatsInProductionCode enforces the "never use floats for money" rule
// instead of relying on everyone remembering it: it fails if float32,
// float64 or strconv.ParseFloat appears anywhere in non-test Go code.
//
// It inspects the syntax tree, not the text, so comments explaining why
// floats are banned don't trip it. It can't catch floats created implicitly,
// e.g. json.Unmarshal into map[string]any; that is why every money field
// uses money.Amount.
func TestNoFloatsInProductionCode(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	banned := map[string]bool{"float32": true, "float64": true, "ParseFloat": true}

	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && banned[id.Name] {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s uses %s: money must be integer cents (see internal/money)", rel, id.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no Go files checked; the repo root is probably wrong")
	}
}
