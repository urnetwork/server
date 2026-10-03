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

// The marked writer must reject a failed endpoint lock and a deadline crossed
// during that lock before queuing either durable row. This guard reviews its
// actual control flow; counts alone cannot establish either error boundary.
func contractLifecycleRedisWriterOrder(source string) error {
	file, err := parser.ParseFile(token.NewFileSet(), "redis.go", "package model\n"+source, 0)
	if err != nil {
		return err
	}
	if len(file.Decls) != 1 {
		return fmt.Errorf("expected one writer")
	}
	fn, ok := file.Decls[0].(*ast.FuncDecl)
	if !ok || fn.Body == nil {
		return fmt.Errorf("missing writer body")
	}
	ident := func(expr ast.Expr, want string) bool { n, ok := expr.(*ast.Ident); return ok && n.Name == want }
	checked := func(stmt ast.Stmt, name string, args []string) bool {
		check, ok := stmt.(*ast.IfStmt)
		if !ok || check.Else != nil || len(check.Body.List) != 1 {
			return false
		}
		assign, ok := check.Init.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 || !ident(assign.Lhs[0], "err") {
			return false
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok || !ident(call.Fun, name) || len(call.Args) != len(args) {
			return false
		}
		for i, arg := range args {
			if !ident(call.Args[i], arg) {
				return false
			}
		}
		condition, ok := check.Cond.(*ast.BinaryExpr)
		if !ok || condition.Op != token.NEQ || !ident(condition.X, "err") || !ident(condition.Y, "nil") {
			return false
		}
		ret, ok := check.Body.List[0].(*ast.ReturnStmt)
		return ok && len(ret.Results) == 3 && ident(ret.Results[0], "nil") && ident(ret.Results[1], "nil") && ident(ret.Results[2], "err")
	}
	var lock, deadline, write token.Pos
	calls, writes, jumps := 0, 0, false
	for _, stmt := range fn.Body.List {
		if checked(stmt, "lockActiveContractClientsInTx", []string{"ctx", "tx", "sourceNetworkId", "sourceId", "destinationNetworkId", "destinationId"}) {
			lock = stmt.Pos()
		}
		if checked(stmt, "validateProberShardAdmissionDeadlineInTx", []string{"ctx", "tx", "deadline"}) {
			deadline = stmt.Pos()
		}
		ast.Inspect(stmt, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.BranchStmt:
				jumps = jumps || n.Tok == token.GOTO
			case *ast.CallExpr:
				if ident(n.Fun, "lockActiveContractClientsInTx") {
					calls++
				}
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					v, e := strconv.Unquote(n.Value)
					if e == nil && strings.Contains(strings.ToUpper(v), "INSERT INTO TRANSFER_CONTRACT") {
						writes++
						write = stmt.Pos()
						if !strings.Contains(v, "clock_timestamp() AT TIME ZONE 'UTC'") {
							write = 0
						}
					}
				}
			}
			return true
		})
	}
	if calls != 1 || writes != 1 || lock == 0 || deadline <= lock || write <= deadline || jumps {
		return fmt.Errorf("want checked endpoint lock, checked deadline, then one database-clock writer")
	}
	return nil
}

func TestRedisContractClockGuardRejectsBypassedLocksAndErrors(t *testing.T) {
	raw, err := os.ReadFile("subscription_redis_admission.go")
	if err != nil {
		t.Fatal(err)
	}
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "redis.go", raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	var source string
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "createRedisTransferEscrowInTx" {
			source = string(raw[fs.Position(fn.Pos()).Offset:fs.Position(fn.End()).Offset])
		}
	}
	if err := contractLifecycleRedisWriterOrder(source); err != nil {
		t.Fatal(err)
	}
	lock := "if err := lockActiveContractClientsInTx(ctx, tx, sourceNetworkId, sourceId, destinationNetworkId, destinationId); err != nil {\n\t\treturn nil, nil, err\n\t}"
	deadline := "if err := validateProberShardAdmissionDeadlineInTx(ctx, tx, deadline); err != nil {\n\t\treturn nil, nil, err\n\t}"
	for name, mutant := range map[string]string{
		"missing_lock":         strings.Replace(source, lock, "", 1),
		"missing_deadline":     strings.Replace(source, deadline, "", 1),
		"unchecked_lock":       strings.Replace(source, lock, "lockActiveContractClientsInTx(ctx, tx, sourceNetworkId, sourceId, destinationNetworkId, destinationId)", 1),
		"wrong_endpoint":       strings.Replace(source, "sourceNetworkId, sourceId, destinationNetworkId, destinationId); err", "sourceNetworkId, sourceId, sourceNetworkId, sourceId); err", 1),
		"swallowed_error":      strings.ReplaceAll(source, "return nil, nil, err", "return nil, nil, nil"),
		"deadline_before_wait": strings.Replace(strings.Replace(source, deadline, "", 1), lock, deadline+"\n"+lock, 1),
		"application_clock":    strings.Replace(source, "clock_timestamp() AT TIME ZONE 'UTC'", "now()", 1),
		"early_write":          strings.Replace(source, lock, "tx.Exec(ctx,`INSERT INTO transfer_contract VALUES(clock_timestamp() AT TIME ZONE 'UTC')`)\n"+lock, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if mutant == source {
				t.Fatal("mutation missed boundary")
			}
			if contractLifecycleRedisWriterOrder(mutant) == nil {
				t.Fatal("unsafe writer accepted")
			}
		})
	}
}
