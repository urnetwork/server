package server

// Which processes reach PgBouncer directly is each one's own code
// (db_pgbouncer_path.go). The deployed services choose the direct path in their
// mains; every other main, the tools operators run through the tunnel that
// forwards only the single port among them, keeps the single port.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The arguments of the SetPgBouncerPath calls in a file's func main.
func pgBouncerPathChoices(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	choices := []string{}
	for _, decl := range file.Decls {
		function, ok := decl.(*ast.FuncDecl)
		if !ok || function.Recv != nil || function.Name.Name != "main" || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "SetPgBouncerPath" {
				return true
			}
			choice := "?"
			if len(call.Args) == 1 {
				if argument, ok := call.Args[0].(*ast.SelectorExpr); ok {
					choice = argument.Sel.Name
				}
			}
			choices = append(choices, choice)
			return true
		})
	}
	return choices
}

// api, connect, proxy, taskworker, alt, gossip and mcp, the services warp
// deploys on the LAN beside the database host, go direct; no other main does.
func TestDeployedServicesChooseTheDirectPgBouncerPath(t *testing.T) {
	serviceMains := map[string]bool{}
	for _, service := range []string{"api", "connect", "proxy", "taskworker", "alt", "gossip", "mcp"} {
		path := filepath.Join("cli", service, "main.go")
		serviceMains[path] = true
		if choices := pgBouncerPathChoices(t, path); !reflect.DeepEqual(choices, []string{"PgBouncerPathDirect"}) {
			t.Error(service, "chooses", choices, "want the direct path once")
		}
	}
	mainCount := 0
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != "." && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "third_party") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || serviceMains[path] {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly)
		if err != nil || file.Name.Name != "main" {
			return nil
		}
		mainCount += 1
		if choices := pgBouncerPathChoices(t, path); len(choices) != 0 {
			t.Error(path, "chooses", choices, "but only the deployed services go direct")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// bringyourctl, the cli tools and the competition binaries among them
	if mainCount < 20 {
		t.Fatal("found only", mainCount, "other main files")
	}
}
