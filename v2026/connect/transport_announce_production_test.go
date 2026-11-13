// Every production announcement belongs to a ConnectHandler, whose reconcile
// holds its row while the transport lives and retires the handler's connected
// rows it does not hold. An exported constructor that announces without the
// handler's reconcile lets a caller announce under a live handler id, and that
// handler's reconcile then retires the live row as a phantom. Only tests may
// build such an announcement.
package connect

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestOnlyTheConnectHandlerAnnouncesInProduction(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	exported := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || !function.Name.IsExported() || function.Type.Results == nil {
				continue
			}
			for _, result := range function.Type.Results.List {
				star, ok := result.Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				if identifier, ok := star.X.(*ast.Ident); ok && identifier.Name == "ConnectionAnnounce" {
					exported = append(exported, name+": "+function.Name.Name)
				}
			}
		}
	}
	slices.Sort(exported)
	if 0 < len(exported) {
		t.Fatalf("exported constructors announce a connection without its handler's reconcile, so a caller under a live handler id gets its live row retired: %s", strings.Join(exported, ", "))
	}
}
