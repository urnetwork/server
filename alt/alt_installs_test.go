package alt

// alt serves api's route set (api.NewRouter on its HTTP/3 front), so it runs what that set runs on as api does
// (EXTENDER.md L1; §19.4's api row): alt's main makes the same install calls as api's, in the same order, the one
// install of api's route set (controller.InstallApiRoutes) among them, over the host each set its process's Env with
// and before it serves. Before, alt's main made none, so over alt the smtp routes, the approval cancel, the
// authorization log, the exchange and the /tls routes answered 503 and §21's hard deny held no network.
//
// alt_installs_route_db_test.go asks the routes themselves through both fronts, over the install.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// What a main under server/cli does about installs, read from its source.
type testMainInstalls struct {
	// each call in main whose callee's name starts with Install, as pkg.Name, in source order
	installs []string
	// the identifier main hands core.SetProcessEnv as its host, and controller.InstallApiRoutes
	envHost     string
	installHost string
	// where main installs the route set's dependencies and where it runs its service
	installPos token.Pos
	runPos     token.Pos
}

// Reads the main of path, whose service runs through runName (api.Run or alt.Run).
func readTestMainInstalls(t testing.TB, path string, runName string) *testMainInstalls {
	t.Helper()
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var mainDecl *ast.FuncDecl
	for _, decl := range file.Decls {
		if funcDecl, ok := decl.(*ast.FuncDecl); ok && funcDecl.Recv == nil && funcDecl.Name.Name == "main" {
			mainDecl = funcDecl
		}
	}
	if mainDecl == nil {
		t.Fatalf("%s has no main", path)
	}
	identifierOf := func(expr ast.Expr) string {
		if ident, ok := expr.(*ast.Ident); ok {
			return ident.Name
		}
		return ""
	}
	view := &testMainInstalls{}
	ast.Inspect(mainDecl.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		qualifier := identifierOf(selector.X)
		name := qualifier + "." + selector.Sel.Name
		if strings.HasPrefix(selector.Sel.Name, "Install") {
			view.installs = append(view.installs, name)
		}
		switch name {
		case "core.SetProcessEnv":
			if len(call.Args) == 2 {
				view.envHost = identifierOf(call.Args[1])
			}
		case "controller.InstallApiRoutes":
			if 2 <= len(call.Args) {
				view.installHost = identifierOf(call.Args[1])
			}
			view.installPos = call.Pos()
		case runName:
			view.runPos = call.Pos()
		}
		return true
	})
	return view
}

// alt's main installs what api's main installs, through the one install of api's route set, over the host it set
// its Env with, before its service serves. Before, alt's main made no install call at all.
func TestAltMainInstallsWhatApisMainInstalls(t *testing.T) {
	apiMain := readTestMainInstalls(t, "../cli/api/main.go", "api.Run")
	altMain := readTestMainInstalls(t, "../cli/alt/main.go", "alt.Run")
	if !slices.Equal(altMain.installs, apiMain.installs) {
		t.Fatalf("alt's main installs %v, api's %v", altMain.installs, apiMain.installs)
	}
	for name, view := range map[string]*testMainInstalls{"api": apiMain, "alt": altMain} {
		if count := len(slices.DeleteFunc(slices.Clone(view.installs), func(install string) bool {
			return install != "controller.InstallApiRoutes"
		})); count != 1 {
			t.Errorf("%s's main installs api's route set %d times: %v", name, count, view.installs)
		}
		if view.envHost == "" || view.installHost != view.envHost {
			t.Errorf("%s's main installs over %q and sets its Env over %q", name, view.installHost, view.envHost)
		}
		if view.runPos == token.NoPos || view.installPos == token.NoPos || view.runPos < view.installPos {
			t.Errorf("%s's main does not install before it serves", name)
		}
	}
}
