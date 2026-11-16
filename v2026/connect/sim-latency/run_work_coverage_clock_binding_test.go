// The source tripwire connects virtual clock controls to the actual protected
// runner without launching its database, fleet, clients or workload.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// This source-only anchor also compiles on the unchanged prior release. A
// missing seam is its intended assertion failure, not a compiler failure.
func TestCoverageRunClockBindsAllLifecycleSites(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var run *ast.FuncDecl
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "Run" {
			run = function
		}
	}
	if run == nil {
		t.Fatal("protected runner definition is missing")
	}
	clockCalls := 0
	ast.Inspect(run.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
			if owner, ok := selector.X.(*ast.Ident); ok && owner.Name == "server" && selector.Sel.Name == "NowUtc" {
				t.Fatal("coverage runner retains direct editable product clock authority")
			}
		}
		name, ok := call.Fun.(*ast.Ident)
		if !ok || name.Name != "runClockNow" {
			return true
		}
		clockCalls++
		if len(call.Args) != 2 {
			t.Fatal("runner clock lost its policy/legacy callback separation")
		}
		policy, policyOk := call.Args[0].(*ast.SelectorExpr)
		product, productOk := call.Args[1].(*ast.SelectorExpr)
		if !policyOk || !productOk {
			t.Fatal("runner clock no longer uses the authenticated run policy and original product function")
		}
		policyOwner, policyOwnerOk := policy.X.(*ast.Ident)
		productOwner, productOwnerOk := product.X.(*ast.Ident)
		if !policyOwnerOk || !productOwnerOk || policyOwner.Name != "options" || policy.Sel.Name != "WorkCoveragePolicy" ||
			productOwner.Name != "server" || product.Sel.Name != "NowUtc" {
			t.Fatal("runner clock substituted another policy, owner or mutable callback")
		}
		return true
	})
	if clockCalls != 4 {
		t.Fatal("coverage clock controls do not bind all four actual lifecycle sites", clockCalls)
	}
	mandatory, err := parser.ParseFile(token.NewFileSet(), "work_coverage_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var deadlineRoot *ast.FuncDecl
	for _, declaration := range mandatory.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "TestWorkCoverageLateDriverDoesNotResetPlannedDeadline" {
			deadlineRoot = function
		}
	}
	if deadlineRoot == nil || len(deadlineRoot.Body.List) < 3 {
		t.Fatal("mandatory G7 deadline root is missing its composed clock controls")
	}
	for index, name := range []string{"assertCoverageRunClockCannotShortenAcceptedMeasurement", "assertCoverageRunClockCannotPruneAcceptedOfferedCohort"} {
		statement, ok := deadlineRoot.Body.List[len(deadlineRoot.Body.List)-2+index].(*ast.ExprStmt)
		if !ok {
			t.Fatal("mandatory G7 clock assertion is not an unconditional tail call", name)
		}
		call, ok := statement.X.(*ast.CallExpr)
		if !ok {
			t.Fatal("mandatory G7 root no longer executes its composed clock assertion", name)
		}
		function, ok := call.Fun.(*ast.Ident)
		if !ok || function.Name != name {
			t.Fatal("new clock files only compile without execution in the mandatory G7 root", name)
		}
	}
}
