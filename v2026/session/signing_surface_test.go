package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// Keep the explicit malformed-token fixture escape hatch out of production.
// The only production direct signer calls stay inside registered mint helpers.
func TestProductionSigningCannotUseFixtureBypass(t *testing.T) {
	root := filepath.Clean("..")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil && file == nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "Testing_Sign" {
				t.Errorf("production fixture-sign bypass: %s", path)
			}
			if call, ok := node.(*ast.CallExpr); ok && filepath.Dir(path) == filepath.Join(root, "session") && filepath.Base(path) != "session_mint.go" && filepath.Base(path) != "by_jwt.go" {
				if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "sign" {
					t.Errorf("unregistered session signing: %s", path)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
