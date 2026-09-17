package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestEnv must not implicitly expose a wildcard profiler or contend for a
// process-global port. Explicit go test CPU/memory profile files need no server.
func TestTestEnvironmentDoesNotStartSharedHTTPProfiler(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "test_util.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, dependency := range file.Imports {
		path, err := strconv.Unquote(dependency.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		if path == "net/http" || path == "net/http/pprof" {
			t.Errorf("TestEnv retains automatic HTTP profiler dependency %q", path)
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		if identifier, ok := node.(*ast.Ident); ok && identifier.Name == "pprofServer" {
			t.Error("TestEnv retains a shared profiler lifecycle")
		}
		return true
	})
}
