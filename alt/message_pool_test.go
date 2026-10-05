package alt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Alt requires live admission dependencies past readiness. Guard the call to
// the shared, natively tested Connect policy without opening those services.
func TestRunUsesSharedConnectMessagePoolPolicy(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	shared, legacy := 0, 0
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runWithDependencies" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if selector.Sel.Name == "ResizeMessagePools" {
				legacy++
			}
			if qualifier, ok := selector.X.(*ast.Ident); ok && qualifier.Name == "connectserver" && selector.Sel.Name == "ConfigureMessagePools" {
				shared++
			}
			return true
		})
	}
	if shared != 1 || legacy != 0 {
		t.Fatalf("Alt shared service policy calls=%d legacy resize calls=%d", shared, legacy)
	}
}
