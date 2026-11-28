package main

// The connect command serves the loopback pprof listener. A start that serves
// needs Postgres and Redis, so the wiring is read from main.go's syntax tree,
// as connect's run tests read run.go (connect/front_run_test.go).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// The command's one connect RunOptions sets LoopbackPprof, so the listener
// serves unless CONNECT_PPROF turns it off.
func TestConnectCommandServesLoopbackPprof(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	runOptionsCount := 0
	loopbackPprof := ""
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok || types.ExprString(literal.Type) != "connectserver.RunOptions" {
			return true
		}
		runOptionsCount++
		for _, element := range literal.Elts {
			if field, ok := element.(*ast.KeyValueExpr); ok && types.ExprString(field.Key) == "LoopbackPprof" {
				loopbackPprof = types.ExprString(field.Value)
			}
		}
		return true
	})
	if runOptionsCount != 1 || loopbackPprof != "true" {
		t.Fatalf("main builds %d connect RunOptions, LoopbackPprof %q; want one, with true", runOptionsCount, loopbackPprof)
	}
}
