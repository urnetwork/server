package main

// all runs api's route set, connect's exchange and taskworker's tasks in one process, so it installs what each of
// their own mains installs, each part once (operatorInstallParts; controller/process_install_parts.go). Before, all's
// main installed nothing: in its process the routes that read the projection, the approval gate's states, the dns
// and exchange scopes, the smtp routes' checks or certs answered 503 and the device grant temporarily_unavailable,
// while sidecar.yml's acl_contract_gate was on its connect node refused every contract and peer list, no §21
// suspension reached the contracts it created, and the worker's abuse, breaker and certs passes ran on nothing.
//
// The tests here read the mains and the parts' lists; installs_route_db_test.go asks the routes themselves.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/taskworker/work"
)

// One call in a function of a main's file: its callee, as qualifier.Name or Name, and the call.
type testFuncCall struct {
	name string
	call *ast.CallExpr
}

// The calls in the function funcName of the file at path, in source order.
func readTestFuncCalls(t *testing.T, path string, funcName string) []testFuncCall {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var funcDecl *ast.FuncDecl
	for _, decl := range file.Decls {
		if decl, ok := decl.(*ast.FuncDecl); ok && decl.Recv == nil && decl.Name.Name == funcName {
			funcDecl = decl
		}
	}
	if funcDecl == nil {
		t.Fatalf("%s has no func %s", path, funcName)
	}
	calls := []testFuncCall{}
	ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			calls = append(calls, testFuncCall{name: callee.Name, call: call})
		case *ast.SelectorExpr:
			if qualifier, ok := callee.X.(*ast.Ident); ok {
				calls = append(calls, testFuncCall{name: qualifier.Name + "." + callee.Sel.Name, call: call})
			}
		}
		return true
	})
	return calls
}

// The first call to name, nil for none.
func testFindCall(calls []testFuncCall, name string) *ast.CallExpr {
	for _, call := range calls {
		if call.name == name {
			return call.call
		}
	}
	return nil
}

// The callees of calls whose name, after the qualifier, starts with Install.
func testInstallCalls(calls []testFuncCall) []string {
	installs := []string{}
	for _, call := range calls {
		_, name, _ := strings.Cut(call.name, ".")
		if name == "" {
			name = call.name
		}
		if strings.HasPrefix(name, "Install") {
			installs = append(installs, call.name)
		}
	}
	return installs
}

// The identifier an argument is, "" for any other expression.
func testIdentifierOf(call *ast.CallExpr, index int) string {
	if call == nil || len(call.Args) <= index {
		return ""
	}
	ident, _ := call.Args[index].(*ast.Ident)
	if ident == nil {
		return ""
	}
	return ident.Name
}

// Each service all runs installs through its own list alone: api's main through its route set's install, connect's
// through its exchange's and taskworker's through its tasks', each once, before its service runs, api's and
// connect's over the host they set their Env with. So whatever a service's main installs is a part of the list all's
// main installs, and a part added to a main beside its list fails here before all could miss it.
func TestServiceMainsInstallThroughTheirListsAlone(t *testing.T) {
	for _, c := range []struct {
		path    string
		install string
		run     string
		// whether the install takes the host the main sets its Env with
		host bool
	}{
		{path: "../api/main.go", install: "controller.InstallApiRoutes", run: "api.Run", host: true},
		{path: "../connect/main.go", install: "controller.InstallConnectExchange", run: "connectserver.Run", host: true},
		{path: "../taskworker/main.go", install: "work.InstallTaskworkerTasks", run: "taskworker.Run"},
	} {
		calls := readTestFuncCalls(t, c.path, "main")
		if installs := testInstallCalls(calls); !slices.Equal(installs, []string{c.install}) {
			t.Errorf("%s installs %v, want %s alone", c.path, installs, c.install)
		}
		install := testFindCall(calls, c.install)
		run := testFindCall(calls, c.run)
		if install == nil || run == nil || run.Pos() < install.Pos() {
			t.Errorf("%s does not install before %s", c.path, c.run)
		}
		if c.host {
			envHost := testIdentifierOf(testFindCall(calls, "core.SetProcessEnv"), 1)
			if envHost == "" || testIdentifierOf(install, 1) != envHost {
				t.Errorf("%s installs over %q and sets its Env over %q", c.path, testIdentifierOf(install, 1), envHost)
			}
		}
	}
}

// all's main installs its services' parts once, through the one list, over the host it set its Env with, before
// supervise starts any service, and installs nothing else.
func TestAllMainInstallsItsServicesPartsBeforeTheyServe(t *testing.T) {
	calls := readTestFuncCalls(t, "main.go", "runCommand")
	if installs := testInstallCalls(calls); !slices.Equal(installs, []string{"controller.InstallProcessParts"}) {
		t.Fatalf("all's main installs %v, want controller.InstallProcessParts alone", installs)
	}
	install := testFindCall(calls, "controller.InstallProcessParts")
	if len(install.Args) != 2 {
		t.Fatalf("all's main installs with %d arguments", len(install.Args))
	}
	parts, _ := install.Args[1].(*ast.CallExpr)
	if parts == nil {
		t.Fatal("all's main installs parts no call lists")
	}
	if partsFunc, _ := parts.Fun.(*ast.Ident); partsFunc == nil || partsFunc.Name != "operatorInstallParts" {
		t.Fatal("all's main installs parts other than operatorInstallParts'")
	}
	envHost := testIdentifierOf(testFindCall(calls, "setProcessEnv"), 0)
	if envHost == "" || testIdentifierOf(parts, 0) != envHost {
		t.Errorf("all's main installs over %q and sets its Env over %q", testIdentifierOf(parts, 0), envHost)
	}
	supervise := testFindCall(calls, "supervise")
	if supervise == nil || supervise.Pos() < install.Pos() {
		t.Error("all's main does not install before supervise starts its services")
	}
}

func testPartNames(parts []controller.ProcessInstallPart) []controller.ProcessInstallPartName {
	names := []controller.ProcessInstallPartName{}
	for _, part := range parts {
		names = append(names, part.Name)
	}
	return names
}

// all's list is its services' own, whole and in their order, and of it all's process installs each part once: api's
// route set's parts, then taskworker's own, §21's parts once for both, and none of connect's, which api's standing and
// authorization cover. Every part a service's main installs is then in all's process, installed or covered by one
// that is, and each service's list installs whole in that service's own process.
func TestAllInstallsEachPartOfItsServicesOnce(t *testing.T) {
	// no part is installed, so the host is never read
	host := &sidecar.ServiceHost{}
	apiParts := controller.ApiRoutesInstallParts(host, nil)
	connectParts := controller.ConnectExchangeInstallParts(host)
	taskworkerParts := work.TaskworkerTasksInstallParts(&work.TaskworkerTasksInstallArgs{FeedDirectory: "feed"})
	parts := operatorInstallParts(host, commandOptions{feedDirectory: "feed"}, nil)
	if names, want := testPartNames(parts), testPartNames(slices.Concat(apiParts, connectParts, taskworkerParts)); !slices.Equal(names, want) {
		t.Fatalf("all's list %v, want its services' %v", names, want)
	}
	for name, list := range map[string][]controller.ProcessInstallPart{"api": apiParts, "connect": connectParts, "taskworker": taskworkerParts} {
		if names, want := testPartNames(controller.SelectProcessInstallParts(list)), testPartNames(list); !slices.Equal(names, want) {
			t.Errorf("%s's own process installs %v of its list %v", name, names, want)
		}
	}

	selected := controller.SelectProcessInstallParts(parts)
	selectedNames := testPartNames(selected)
	want := slices.Concat(testPartNames(apiParts), []controller.ProcessInstallPartName{work.InstallPartMailBreakerSources, work.InstallPartTlsCertChains})
	if !slices.Equal(selectedNames, want) {
		t.Fatalf("all's process installs %v, want %v", selectedNames, want)
	}
	sortedNames := slices.Clone(selectedNames)
	slices.Sort(sortedNames)
	if len(slices.Compact(sortedNames)) != len(selectedNames) {
		t.Fatalf("all's process installs a part twice: %v", selectedNames)
	}
	coveredBy := map[controller.ProcessInstallPartName]controller.ProcessInstallPartName{}
	for _, part := range selected {
		for _, covered := range part.Covers {
			coveredBy[covered] = part.Name
		}
	}
	for name, list := range map[string][]controller.ProcessInstallPart{"api": apiParts, "connect": connectParts, "taskworker": taskworkerParts} {
		for _, part := range list {
			if !slices.Contains(selectedNames, part.Name) && coveredBy[part.Name] == "" {
				t.Errorf("%s's part %s is neither installed nor covered in all's process", name, part.Name)
			}
		}
	}
	for covered, cover := range map[controller.ProcessInstallPartName]controller.ProcessInstallPartName{
		controller.InstallPartAbuseHardDeny:        controller.InstallPartAbuseStanding,
		controller.InstallPartConnectAuthorization: controller.InstallPartApiAuthorization,
	} {
		if coveredBy[covered] != cover {
			t.Errorf("connect's %s is covered by %q, want %s", covered, coveredBy[covered], cover)
		}
	}
}

// --feed-dir is taskworker's --feed_dir, which the breakers' measures read: none by default, as taskworker's.
func TestParseCommandFeedDirectory(t *testing.T) {
	options, err := parseCommand([]string{"run", "--feed-dir=/srv/feed-mirror"})
	if err != nil {
		t.Fatal(err)
	}
	if options.feedDirectory != "/srv/feed-mirror" {
		t.Errorf("feed directory = %q", options.feedDirectory)
	}
	options, err = parseCommand(nil)
	if err != nil || options.feedDirectory != "" {
		t.Errorf("default feed directory = %q, %v", options.feedDirectory, err)
	}
	if !strings.Contains(usage, "--feed-dir=") {
		t.Error("usage does not offer --feed-dir")
	}
}
