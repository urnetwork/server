package model

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// This is intentionally a guard for the reviewed writer shape, not a general
// Go control-flow analyzer. Every nonnegative byte count must pass a checked
// endpoint lock. Zero locks precede the payer wait; positive locks follow it;
// the sole direct INSERT follows both. A new shape requires explicit review.
func contractLifecycleEscrowBranchLockOrder(source string) error {
	f, err := parser.ParseFile(token.NewFileSet(), "writer.go", "package model\n"+source, 0)
	if err != nil {
		return err
	}
	if len(f.Decls) != 1 {
		return fmt.Errorf("expected one escrow writer")
	}
	function, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok || function.Body == nil {
		return fmt.Errorf("expected escrow function body")
	}
	ident := func(expr ast.Expr, name string) bool {
		node, ok := expr.(*ast.Ident)
		return ok && node.Name == name
	}
	zero := func(expr ast.Expr) bool {
		node, ok := expr.(*ast.BasicLit)
		return ok && node.Kind == token.INT && node.Value == "0"
	}
	comparison := func(expr ast.Expr, operator token.Token, zeroFirst bool) bool {
		binary, ok := expr.(*ast.BinaryExpr)
		if !ok || binary.Op != operator {
			return false
		}
		if zeroFirst {
			return zero(binary.X) && ident(binary.Y, "contractTransferByteCount")
		}
		return ident(binary.X, "contractTransferByteCount") && zero(binary.Y)
	}
	checkedLock := func(statement ast.Stmt) bool {
		check, ok := statement.(*ast.IfStmt)
		if !ok || check.Else != nil || len(check.Body.List) != 1 {
			return false
		}
		assignment, ok := check.Init.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != 1 || !ident(assignment.Lhs[0], "err") || len(assignment.Rhs) != 1 {
			return false
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok || !ident(call.Fun, "lockActiveContractClientsInTx") {
			return false
		}
		arguments := []string{"ctx", "tx", "sourceNetworkId", "sourceId", "destinationNetworkId", "destinationId"}
		if len(call.Args) != len(arguments) {
			return false
		}
		for i, name := range arguments {
			if !ident(call.Args[i], name) {
				return false
			}
		}
		condition, ok := check.Cond.(*ast.BinaryExpr)
		if !ok || condition.Op != token.NEQ || !ident(condition.X, "err") || !ident(condition.Y, "nil") {
			return false
		}
		returned, ok := check.Body.List[0].(*ast.ReturnStmt)
		return ok && len(returned.Results) == 3 && ident(returned.Results[2], "err")
	}
	var negative, anchor, positive *ast.IfStmt
	var financial ast.Stmt
	var write *ast.ExprStmt
	locks, writes, waits, jumps := 0, 0, 0, false
	for _, statement := range function.Body.List {
		if conditional, ok := statement.(*ast.IfStmt); ok && conditional.Init == nil && conditional.Else == nil {
			switch {
			case comparison(conditional.Cond, token.LSS, false):
				if negative != nil {
					return fmt.Errorf("duplicate negative-byte branch")
				}
				negative = conditional
			case comparison(conditional.Cond, token.EQL, false):
				if anchor != nil {
					return fmt.Errorf("duplicate zero-byte branch")
				}
				anchor = conditional
			case comparison(conditional.Cond, token.LSS, true):
				// Positive-byte preflight guards and later accounting branches
				// do not own the lifecycle lock. Select the branch containing
				// that exact call, then validate its checked first statement and
				// position after the financial wait below.
				hasLock := false
				ast.Inspect(conditional.Body, func(node ast.Node) bool {
					if call, ok := node.(*ast.CallExpr); ok && ident(call.Fun, "lockActiveContractClientsInTx") {
						hasLock = true
					}
					return true
				})
				if hasLock {
					if positive != nil {
						return fmt.Errorf("duplicate positive-byte lifecycle lock branch")
					}
					positive = conditional
				}
			}
		}
		ast.Inspect(statement, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.BranchStmt:
				jumps = jumps || node.Tok == token.GOTO
			case *ast.CallExpr:
				if ident(node.Fun, "lockActiveContractClientsInTx") {
					locks++
				}
				if ident(node.Fun, "loadTransferEscrowBalances") {
					waits++
					financial = statement
				}
			case *ast.BasicLit:
				if node.Kind != token.STRING {
					break
				}
				value, err := strconv.Unquote(node.Value)
				if err == nil && strings.Contains(strings.ToUpper(strings.Join(strings.Fields(value), " ")), "INSERT INTO TRANSFER_CONTRACT") {
					writes++
					write, _ = statement.(*ast.ExprStmt)
				}
			}
			return true
		})
	}
	if negative == nil || anchor == nil || positive == nil || financial == nil || write == nil || locks != 2 || waits != 1 || writes != 1 || jumps {
		return fmt.Errorf("unreviewed escrow branch/write shape")
	}
	if len(negative.Body.List) != 1 {
		return fmt.Errorf("negative bytes must return before either lock branch")
	}
	if returned, ok := negative.Body.List[0].(*ast.ReturnStmt); !ok || len(returned.Results) != 3 || ident(returned.Results[2], "nil") {
		return fmt.Errorf("negative bytes must return an error")
	}
	if len(anchor.Body.List) != 1 || !checkedLock(anchor.Body.List[0]) || len(positive.Body.List) == 0 || !checkedLock(positive.Body.List[0]) {
		return fmt.Errorf("each nonnegative branch must check and propagate its endpoint-lock error")
	}
	if !(negative.End() < anchor.Pos() && anchor.End() < financial.Pos() && financial.End() < positive.Pos() && positive.End() < write.Pos()) {
		return fmt.Errorf("want negative rejection, zero lock, financial wait, positive lock, then INSERT")
	}
	return nil
}

func TestContractLifecycleEscrowBranchClockGuardRejectsMissingOrLateLocks(t *testing.T) {
	data, err := os.ReadFile("subscription_model.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "subscription_model.go", data, 0)
	if err != nil {
		t.Fatal(err)
	}
	var source string
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "createTransferEscrowInTx" {
			source = string(data[fset.Position(function.Pos()).Offset:fset.Position(function.End()).Offset])
		}
	}
	if err := contractLifecycleEscrowBranchLockOrder(source); err != nil {
		t.Fatalf("reviewed current writer: %v", err)
	}
	lock := "lockActiveContractClientsInTx("
	lastLock := strings.LastIndex(source, lock)
	// Move the existing positive branch after the existing INSERT, preserving
	// exactly two lock calls and one writer. Counts alone must not accept it.
	mutantSet := token.NewFileSet()
	prefix := "package model\n"
	parsed, err := parser.ParseFile(mutantSet, "writer.go", prefix+source, 0)
	if err != nil {
		t.Fatal(err)
	}
	position := func(pos token.Pos) int { return mutantSet.Position(pos).Offset - len(prefix) }
	positiveStart, positiveEnd, writeEnd := 0, 0, 0
	for _, statement := range parsed.Decls[0].(*ast.FuncDecl).Body.List {
		start, end := position(statement.Pos()), position(statement.End())
		text := source[start:end]
		if strings.HasPrefix(text, "if 0 < contractTransferByteCount {") && strings.Contains(text, lock) {
			positiveStart, positiveEnd = start, end
		}
		if _, ok := statement.(*ast.ExprStmt); ok && strings.Contains(text, "INSERT INTO transfer_contract") {
			writeEnd = end
		}
	}
	if positiveStart == 0 || positiveEnd >= writeEnd {
		t.Fatal("expected existing positive branch before direct INSERT")
	}
	lateLock := source[:positiveStart] + source[positiveEnd:writeEnd] + "\n" + source[positiveStart:positiveEnd] + source[writeEnd:]
	mutants := map[string]string{
		"existing positive lock after write": lateLock,
		"zero lock missing":                  strings.Replace(source, lock, "omittedEndpointLock(", 1),
		"positive lock missing":              source[:lastLock] + strings.Replace(source[lastLock:], lock, "omittedEndpointLock(", 1),
		"zero coverage hole":                 strings.Replace(source, "if contractTransferByteCount == 0 {", "if contractTransferByteCount == 1 {", 1),
		"positive coverage hole":             source[:positiveStart] + strings.Replace(source[positiveStart:positiveEnd], "if 0 < contractTransferByteCount {", "if 1 < contractTransferByteCount {", 1) + source[positiveEnd:],
		"negative rejection missing":         strings.Replace(source, "if contractTransferByteCount < 0 {", "if contractTransferByteCount < -1 {", 1),
		"lock error ignored":                 strings.Replace(source, "return nil, nil, err", "return nil, nil, nil", -1),
		"different endpoint locked":          strings.Replace(source, "lockActiveContractClientsInTx(ctx, tx, sourceNetworkId, sourceId, destinationNetworkId, destinationId)", "lockActiveContractClientsInTx(ctx, tx, sourceNetworkId, sourceId, sourceNetworkId, sourceId)", 1),
		"early write":                        strings.Replace(source, "if contractTransferByteCount == 0 {", "tx.Exec(\"INSERT INTO transfer_contract VALUES (clock_timestamp() AT TIME ZONE 'UTC')\")\nif contractTransferByteCount == 0 {", 1),
	}
	for name, mutant := range mutants {
		t.Run(name, func(t *testing.T) {
			if mutant == source {
				t.Fatal("mutation did not change the reviewed writer")
			}
			if err := contractLifecycleEscrowBranchLockOrder(mutant); err == nil {
				t.Fatal("accepted a writer that bypasses the lifecycle lock contract")
			}
		})
	}
}
