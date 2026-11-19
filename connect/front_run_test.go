package connect

// connect's start and drain with the lb_alt front, as run.go wires them. A
// start that serves needs Postgres and Redis, which these tests don't reach,
// so the wiring is read from run.go's syntax tree, as alt's run test does
// (alt/exit_profile_test.go).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"testing"
)

// One call in runWithDependencies, with the nodes that enclose it, outermost
// first.
type testFrontRunCall struct {
	call      *ast.CallExpr
	ancestors []ast.Node
}

// Whether the call runs on runWithDependencies' own goroutine, in order: not
// in a go statement, a deferred call or a function literal.
func (self testFrontRunCall) inline() bool {
	for _, ancestor := range self.ancestors {
		switch ancestor.(type) {
		case *ast.GoStmt, *ast.DeferStmt, *ast.FuncLit:
			return false
		}
	}
	return true
}

// The node that directly encloses the call.
func (self testFrontRunCall) parent() ast.Node {
	return self.ancestors[len(self.ancestors)-1]
}

// Every call in runWithDependencies, by the text of the called function.
func testFrontRunCalls(t *testing.T) map[string][]testFrontRunCall {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var run *ast.FuncDecl
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "runWithDependencies" {
			run = function
		}
	}
	if run == nil {
		t.Fatal("no runWithDependencies")
	}
	functionCalls := map[string][]testFrontRunCall{}
	ancestors := []ast.Node{}
	ast.Inspect(run.Body, func(node ast.Node) bool {
		if node == nil {
			ancestors = ancestors[:len(ancestors)-1]
			return true
		}
		if call, ok := node.(*ast.CallExpr); ok {
			function := types.ExprString(call.Fun)
			functionCalls[function] = append(functionCalls[function], testFrontRunCall{
				call:      call,
				ancestors: slices.Clone(ancestors),
			})
		}
		ancestors = append(ancestors, node)
		return true
	})
	return functionCalls
}

// The one call of function, or a failure.
func testFrontRunOneCall(t *testing.T, functionCalls map[string][]testFrontRunCall, function string) testFrontRunCall {
	t.Helper()
	calls := functionCalls[function]
	if len(calls) != 1 {
		t.Fatalf("runWithDependencies calls %s %d times, want once", function, len(calls))
	}
	return calls[0]
}

// The texts of a call's arguments.
func testFrontRunArgs(call *ast.CallExpr) []string {
	args := []string{}
	for _, arg := range call.Args {
		args = append(args, types.ExprString(arg))
	}
	return args
}

// The front loads before readiness, failing the start; it starts on the
// start's own goroutine, its failure failing the start, before the private
// listener first answers /status; /status reads it; and the drain on the
// caller's cancel drains it, then cancels.
func TestRunStartsTheFrontBeforeTheStatusListenerAndDrainsIt(t *testing.T) {
	functionCalls := testFrontRunCalls(t)

	load := testFrontRunOneCall(t, functionCalls, "newFrontServerFromEnv")
	readiness := testFrontRunOneCall(t, functionCalls, "readiness")
	if !load.inline() || readiness.call.Pos() < load.call.Pos() {
		t.Fatal("the front does not load on the start's goroutine before readiness")
	}

	start := testFrontRunOneCall(t, functionCalls, "publicFront.startWithRouter")
	if !start.inline() {
		t.Fatal("the front starts off the start's goroutine, so /status can answer before its sockets bind")
	}
	startAssign, ok := start.parent().(*ast.AssignStmt)
	if !ok || len(startAssign.Lhs) != 1 || types.ExprString(startAssign.Lhs[0]) != "err" {
		t.Fatal("the front's start error is not kept")
	}
	startIf, ok := start.ancestors[len(start.ancestors)-2].(*ast.IfStmt)
	if !ok || startIf.Init != startAssign || types.ExprString(startIf.Cond) != "err != nil" {
		t.Fatal("the front's start error is not checked")
	}
	returned := false
	for _, statement := range startIf.Body.List {
		if returnStmt, ok := statement.(*ast.ReturnStmt); ok && len(returnStmt.Results) == 1 {
			ast.Inspect(returnStmt.Results[0], func(node ast.Node) bool {
				if ident, ok := node.(*ast.Ident); ok && ident.Name == "err" {
					returned = true
				}
				return true
			})
		}
	}
	if !returned {
		t.Fatal("a failed front start does not fail connect's start")
	}

	listen := testFrontRunOneCall(t, functionCalls, "listenAndServe")
	if !listen.inline() || listen.call.Pos() < start.call.Pos() {
		t.Fatal("the private listener can answer /status before the front's sockets bind")
	}

	status := testFrontRunOneCall(t, functionCalls, "connectRouter.statusWithWarpStatus")
	if args := testFrontRunArgs(status.call); len(args) != 4 || args[3] != "publicFront" {
		t.Fatalf("the private /status reads %v, not the front", args)
	}

	drain := testFrontRunOneCall(t, functionCalls, "drainConnect")
	args := testFrontRunArgs(drain.call)
	if len(args) != 4 || args[0] != "options.WarpStatus" || args[1] != "publicFront" || args[3] != "cancel" {
		t.Fatalf("the drain is drainConnect(%v), want the status latch, the front, the exchange drain and the cancel", args)
	}
	onDone := false
	for _, ancestor := range drain.ancestors {
		if clause, ok := ancestor.(*ast.CommClause); ok {
			if receive, ok := clause.Comm.(*ast.ExprStmt); ok && types.ExprString(receive.X) == "<-ctx.Done()" {
				onDone = true
			}
		}
	}
	if !onDone {
		t.Fatal("the drain does not run on the caller's cancel")
	}
}
