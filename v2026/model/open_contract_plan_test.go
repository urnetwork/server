package model

// open_contract_plan_test.go reproduces the false-zero generated-open planner
// state. Open rows are written before ANALYZE but held outside its MVCC
// snapshot, so PostgreSQL records the generated value and every open-partial
// index as empty even though those rows become visible before EXPLAIN. The hot
// pair and payer queries must still select their isolated structural indexes.

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

const (
	openContractPlanClosedRows   = 100000
	openContractPlanPairRows     = 1000
	openContractPlanEndpointRows = 1000

	openContractSourcePairIndex      = "transfer_contract_unresolved_source_pair_create_time"
	openContractDestinationPairIndex = "transfer_contract_unresolved_destination_pair_create_time"
	openContractPayerIndex           = "transfer_contract_unresolved_payer_transfer_byte_count"
	openContractGenericIndex         = "transfer_contract_open_partial_create_time"
	openContractOutcomeNullIndex     = "transfer_contract_outcome_null"
)

// Resolve the real query owner only through the public wrapper's returned
// call. A dead helper or an unused callback cannot satisfy the SQL guard.
func openContractCompanionQueryOwner(file *ast.File) (*ast.FuncDecl, error) {
	functions := map[string]*ast.FuncDecl{}
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil {
			functions[function.Name.Name] = function
		}
	}
	ident := func(expression ast.Expr, name string) bool {
		value, ok := expression.(*ast.Ident)
		return ok && value.Name == name
	}
	returnedCall := func(body *ast.BlockStmt) *ast.CallExpr {
		if body == nil || len(body.List) != 1 {
			return nil
		}
		statement, ok := body.List[0].(*ast.ReturnStmt)
		if !ok || len(statement.Results) != 1 {
			return nil
		}
		call, _ := statement.Results[0].(*ast.CallExpr)
		return call
	}
	wrapper := functions["CreateCompanionTransferEscrow"]
	owner := functions["createCompanionTransferEscrow"]
	if wrapper == nil || owner == nil || owner.Body == nil {
		return nil, fmt.Errorf("missing public companion wrapper or actual query owner")
	}
	wrapperBody := wrapper.Body
	if wrapperBody != nil && len(wrapperBody.List) == 2 {
		// The public owner pins its optional work source before Redis may
		// replay the callback. Admit only that exact context-preserving step.
		assignment, ok := wrapperBody.List[0].(*ast.AssignStmt)
		if !ok || assignment.Tok != token.ASSIGN || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 || !ident(assignment.Lhs[0], "ctx") {
			return nil, fmt.Errorf("companion wrapper has an unexpected pre-admission statement")
		}
		pin, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok || !ident(pin.Fun, "providerWorkSessionContext") || len(pin.Args) != 1 || !ident(pin.Args[0], "ctx") {
			return nil, fmt.Errorf("companion wrapper changed its pinned caller context")
		}
		wrapperBody = &ast.BlockStmt{List: wrapperBody.List[1:]}
	}
	call := returnedCall(wrapperBody)
	if call == nil {
		return nil, fmt.Errorf("public companion wrapper does not return its query owner")
	}
	direct := true
	if ident(call.Fun, "runRedisContractAdmission") {
		direct = false
		if len(call.Args) != 2 || !ident(call.Args[0], "ctx") {
			return nil, fmt.Errorf("companion recovery owner lost caller context")
		}
		callback, ok := call.Args[1].(*ast.FuncLit)
		if !ok || callback.Type.Params == nil || len(callback.Type.Params.List) != 1 || len(callback.Type.Params.List[0].Names) != 1 || callback.Type.Params.List[0].Names[0].Name != "ctx" {
			return nil, fmt.Errorf("companion recovery callback lost its owned context")
		}
		call = returnedCall(callback.Body)
	}
	arguments := []string{"ctx", "sourceNetworkId", "sourceId", "destinationNetworkId", "destinationId", "contractTransferByteCount", "originContractTimeout"}
	if call == nil || !ident(call.Fun, owner.Name.Name) || len(call.Args) != len(arguments) {
		return nil, fmt.Errorf("public companion wrapper is disconnected from the guarded query owner")
	}
	for index, name := range arguments {
		expression := call.Args[index]
		if index == 0 && direct {
			admission, ok := expression.(*ast.CallExpr)
			if !ok || !ident(admission.Fun, "withRedisContractAdmission") || len(admission.Args) != 1 {
				return nil, fmt.Errorf("direct companion query lost admission context")
			}
			expression = admission.Args[0]
		}
		if !ident(expression, name) {
			return nil, fmt.Errorf("companion query argument %s changed at public wrapper", name)
		}
	}
	return owner, nil
}

// Binds the planner regression below to the actual runtime statements. The
// database fixture proves why the generated open column is unsafe for these
// pair/payer lookups; this source-level guard prevents a later cleanup from
// putting that statistics-dependent predicate back while leaving the
// independently written EXPLAIN fixtures green.
func TestOpenContractRuntimeQueriesUseStructuralPredicate(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "subscription_model.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := openContractCompanionQueryOwner(file)
	if err != nil {
		t.Fatal(err)
	}
	// These are the two executed tx.Query literals, not independent fixture
	// strings or unreachable declarations. Each has both discovery predicates.
	queries := map[string]bool{"AS earliest_origin": false, "AS earliest_companion_origin": false}
	ast.Inspect(owner.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Query" {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		literal, literalOk := call.Args[1].(*ast.BasicLit)
		if !ok || receiver.Name != "tx" || !literalOk || literal.Kind != token.STRING {
			return true
		}
		sql, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatal(err)
		}
		for marker := range queries {
			if strings.Contains(sql, marker) {
				queries[marker] = true
				normalized := strings.ToLower(strings.Join(strings.Fields(sql), " "))
				if strings.Count(normalized, "case when outcome is null then dispute = false else false end") < 2 {
					t.Errorf("actual companion query %s lost its two structural CASE predicates", marker)
				}
			}
		}
		return true
	})
	for marker, found := range queries {
		if !found {
			t.Errorf("actual companion tx.Query %s not found", marker)
		}
	}

	minimumPredicates := map[string]int{
		"createCompanionTransferEscrow":                     2,
		"GetOpenTransferEscrowsOrderedByPriorityCreateTime": 1,
		"GetOpenContractIds":                                1,
		"GetOpenContractIdsForSourceOrDestination":          1,
		"GetOpenTransferByteCount":                          1,
	}
	found := map[string]bool{}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		minimum, guarded := minimumPredicates[function.Name.Name]
		if !guarded {
			continue
		}
		found[function.Name.Name] = true

		literals := []string{}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("unquote %s string literal: %v", function.Name.Name, err)
			}
			literals = append(literals, value)
			return true
		})

		sql := strings.ToLower(strings.Join(strings.Fields(strings.Join(literals, " ")), " "))
		compactSql := strings.ReplaceAll(sql, " ", "")
		if strings.Contains(compactSql, "open=true") {
			t.Errorf("%s still contains the statistics-dependent open=true predicate", function.Name.Name)
		}
		if count := strings.Count(sql, "dispute = false"); count < minimum {
			t.Errorf("%s structural dispute predicates = %d, want at least %d", function.Name.Name, count, minimum)
		}
		if count := strings.Count(sql, "outcome is null"); count < minimum {
			t.Errorf("%s structural outcome predicates = %d, want at least %d", function.Name.Name, count, minimum)
		}
	}

	for function := range minimumPredicates {
		if !found[function] {
			t.Errorf("guarded runtime function %s not found", function)
		}
	}
}

// Guards every pair/payer runtime shape while preserving the global close
// poll's intentional use of the ordered open index.
func TestOpenContractPlansIgnoreFalseZeroGeneratedOpenStats(t *testing.T) {
	if testing.Short() {
		t.Skip("open-contract query-plan test seeds 100k rows; skipped in -short")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sourceId := server.NewId()
		destinationId := server.NewId()
		endpointId := server.NewId()
		payerNetworkId := server.NewId()

		seedFalseZeroOpenContractStats(t, ctx, sourceId, destinationId, endpointId, payerNetworkId)

		server.Db(ctx, func(conn server.PgConn) {
			assertOpenGeneratedExpressionEquivalent(t, ctx, conn)
			assertOpenPlanStatsAreFalseZero(t, ctx, conn)

			pairPlan := explainOpenContractPlan(t, ctx, conn, `
				SELECT transfer_contract.contract_id, contract_close.party, contract_close.checkpoint
				FROM transfer_contract
				LEFT JOIN contract_close ON contract_close.contract_id = transfer_contract.contract_id
				WHERE (CASE WHEN transfer_contract.outcome IS NULL THEN transfer_contract.dispute = false ELSE false END)
				  AND transfer_contract.source_id = $1
				  AND transfer_contract.destination_id = $2
			`, sourceId, destinationId)
			assertPlanUsesAnyIndex(t, pairPlan, openContractSourcePairIndex, openContractDestinationPairIndex)
			assertPlanAvoidsIndexes(t, pairPlan, openContractGenericIndex, openContractOutcomeNullIndex, openContractPayerIndex)

			originPlan := explainOpenContractPlan(t, ctx, conn, `
				SELECT contract_id
				FROM (
					(
						SELECT contract_id, create_time
						FROM transfer_contract
						WHERE (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END)
						  AND source_id = $1
						  AND destination_id = $2
						  AND companion_contract_id IS NULL
						ORDER BY create_time ASC
						LIMIT 1
					)
					UNION ALL
					(
						SELECT contract_id, create_time
						FROM transfer_contract
						WHERE open = false
						  AND $3 <= close_time
						  AND source_id = $1
						  AND destination_id = $2
						  AND companion_contract_id IS NULL
						ORDER BY create_time ASC
						LIMIT 1
					)
					ORDER BY create_time ASC
					LIMIT 1
				) AS earliest_origin
			`, sourceId, destinationId, server.NowUtc().Add(-time.Hour))
			assertPlanUsesAnyIndex(t, originPlan, openContractSourcePairIndex, openContractDestinationPairIndex)
			assertPlanAvoidsIndexes(t, originPlan, openContractGenericIndex, openContractOutcomeNullIndex, openContractPayerIndex)

			escrowPlan := explainOpenContractPlan(t, ctx, conn, `
				SELECT transfer_contract.contract_id,
				       transfer_contract.transfer_byte_count,
				       transfer_contract.priority
				FROM transfer_contract
				LEFT JOIN contract_close ON contract_close.contract_id = transfer_contract.contract_id
				INNER JOIN transfer_escrow ON transfer_escrow.contract_id = transfer_contract.contract_id
				WHERE (CASE WHEN transfer_contract.outcome IS NULL THEN transfer_contract.dispute = false ELSE false END)
				  AND transfer_contract.source_id = $1
				  AND transfer_contract.destination_id = $2
				  AND transfer_contract.transfer_byte_count <= $3
				  AND contract_close.contract_id IS NULL
				ORDER BY transfer_contract.priority DESC, transfer_contract.create_time ASC
			`, sourceId, destinationId, ByteCount(1024))
			assertPlanUsesAnyIndex(t, escrowPlan, openContractSourcePairIndex, openContractDestinationPairIndex)
			assertPlanAvoidsIndexes(t, escrowPlan, openContractGenericIndex, openContractOutcomeNullIndex, openContractPayerIndex)

			endpointPlan := explainOpenContractPlan(t, ctx, conn, `
				SELECT transfer_contract.source_id,
				       transfer_contract.destination_id,
				       transfer_contract.contract_id,
				       contract_close.party,
				       contract_close.checkpoint
				FROM transfer_contract
				LEFT JOIN contract_close ON contract_close.contract_id = transfer_contract.contract_id
				WHERE (CASE WHEN transfer_contract.outcome IS NULL THEN transfer_contract.dispute = false ELSE false END)
				  AND (transfer_contract.source_id = $1 OR transfer_contract.destination_id = $1)
			`, endpointId)
			assertPlanUsesIndex(t, endpointPlan, openContractSourcePairIndex)
			assertPlanUsesIndex(t, endpointPlan, openContractDestinationPairIndex)
			assertPlanAvoidsIndexes(t, endpointPlan, openContractGenericIndex, openContractOutcomeNullIndex, openContractPayerIndex)

			payerPlan := explainOpenContractPlan(t, ctx, conn, `
				SELECT COALESCE(SUM(transfer_byte_count), 0)
				FROM transfer_contract
				WHERE payer_network_id = $1
				  AND (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END)
			`, payerNetworkId)
			assertPlanUsesIndex(t, payerPlan, openContractPayerIndex)
			assertPlanAvoidsIndexes(t, payerPlan, openContractGenericIndex, openContractOutcomeNullIndex, openContractSourcePairIndex, openContractDestinationPairIndex)

			globalPlan := explainOpenContractPlan(t, ctx, conn, `
				SELECT contract_id
				FROM transfer_contract
				WHERE open AND create_time <= $1
				ORDER BY create_time
				LIMIT 25000
			`, server.NowUtc())
			assertPlanUsesIndex(t, globalPlan, openContractGenericIndex)
		})
	})
}

// Builds the production catalog failure deterministically without relying on
// ANALYZE's randomized sample. The open rows already exist physically when the
// second connection analyzes, but remain invisible until its false-only sample
// and all partial-index estimates have committed.
func seedFalseZeroOpenContractStats(
	t testing.TB,
	ctx context.Context,
	sourceId server.Id,
	destinationId server.Id,
	endpointId server.Id,
	payerNetworkId server.Id,
) {
	t.Helper()
	server.Db(ctx, func(writerConn server.PgConn) {
		server.RaisePgResult(writerConn.Exec(ctx, `ALTER TABLE transfer_contract SET (autovacuum_enabled = false)`))
		server.RaisePgResult(writerConn.Exec(ctx, `
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id,
				destination_network_id, destination_id,
				transfer_byte_count, create_time, close_time,
				payer_network_id, outcome
			)
			SELECT gen_random_uuid(), gen_random_uuid(), gen_random_uuid(),
			       gen_random_uuid(), gen_random_uuid(), 1, now(), now(),
			       gen_random_uuid(), 'success'
			FROM generate_series(1, $1)
		`, openContractPlanClosedRows))

		writerTx, err := writerConn.Begin(ctx)
		server.Raise(err)
		committed := false
		defer func() {
			if !committed {
				_ = writerTx.Rollback(ctx)
			}
		}()

		server.RaisePgResult(writerTx.Exec(ctx, `
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id,
				destination_network_id, destination_id,
				transfer_byte_count, create_time, payer_network_id,
				companion_contract_id
			)
			SELECT gen_random_uuid(), gen_random_uuid(), $1,
			       gen_random_uuid(), $2, 1, now() + g * interval '1 microsecond', $3,
			       CASE WHEN g % 3 = 0 THEN gen_random_uuid() ELSE NULL END
			FROM generate_series(1, $4) AS g
		`, sourceId, destinationId, payerNetworkId, openContractPlanPairRows))
		server.RaisePgResult(writerTx.Exec(ctx, `
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id,
				destination_network_id, destination_id,
				transfer_byte_count, create_time, payer_network_id
			)
			SELECT gen_random_uuid(), gen_random_uuid(),
			       CASE WHEN g % 2 = 0 THEN $1 ELSE gen_random_uuid() END,
			       gen_random_uuid(),
			       CASE WHEN g % 2 = 0 THEN gen_random_uuid() ELSE $1 END,
			       1, now() + g * interval '1 microsecond', $2
			FROM generate_series(1, $3) AS g
		`, endpointId, payerNetworkId, openContractPlanEndpointRows))
		server.RaisePgResult(writerTx.Exec(ctx, `
			INSERT INTO transfer_escrow (contract_id, balance_id, balance_byte_count)
			SELECT contract_id, gen_random_uuid(), transfer_byte_count
			FROM transfer_contract
			WHERE dispute = false AND outcome IS NULL
		`))

		server.Db(ctx, func(analyzeConn server.PgConn) {
			server.RaisePgResult(analyzeConn.Exec(ctx, `ANALYZE transfer_contract`))
		})
		server.Raise(writerTx.Commit(ctx))
		committed = true
	})

	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_escrow`))
	})
}

// Confirms the opaque query predicate preserves the generated column's exact
// truth table.
func assertOpenGeneratedExpressionEquivalent(t testing.TB, ctx context.Context, conn server.PgConn) {
	t.Helper()
	var mismatchCount int
	result, err := conn.Query(ctx, `
		SELECT count(*)
		FROM transfer_contract
		WHERE open IS DISTINCT FROM
		      (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END)
	`)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&mismatchCount))
		}
	})
	if mismatchCount != 0 {
		t.Fatalf("generated open expression mismatches = %d, want 0", mismatchCount)
	}
}

// Proves the fixture reached the same misleading catalog state as the incident.
func assertOpenPlanStatsAreFalseZero(t testing.TB, ctx context.Context, conn server.PgConn) {
	t.Helper()
	var nDistinct float64
	result, err := conn.Query(ctx, `
		SELECT n_distinct
		FROM pg_stats
		WHERE schemaname = current_schema()
		  AND tablename = 'transfer_contract'
		  AND attname = 'open'
	`)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&nDistinct))
		}
	})
	if nDistinct != 1 {
		t.Fatalf("transfer_contract.open n_distinct = %v, want 1", nDistinct)
	}

	indexNames := []string{
		openContractGenericIndex,
		openContractOutcomeNullIndex,
		openContractSourcePairIndex,
		openContractDestinationPairIndex,
		openContractPayerIndex,
	}
	var falseZeroCount int
	result, err = conn.Query(ctx, `
		SELECT count(*)
		FROM pg_class
		WHERE relname = ANY($1::text[]) AND reltuples = 0
	`, indexNames)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&falseZeroCount))
		}
	})
	if falseZeroCount != len(indexNames) {
		t.Fatalf("false-zero target indexes = %d, want %d", falseZeroCount, len(indexNames))
	}
}

// Returns a stable textual plan without executing the deliberately dangerous
// false-zero query.
func explainOpenContractPlan(
	t testing.TB,
	ctx context.Context,
	conn server.PgConn,
	sql string,
	args ...any,
) string {
	t.Helper()
	result, err := conn.Query(ctx, "EXPLAIN (COSTS OFF) "+sql, args...)
	lines := []string{}
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var line string
			server.Raise(result.Scan(&line))
			lines = append(lines, line)
		}
	})
	return strings.Join(lines, "\n")
}

// Requires one named structural path when symmetric pair indexes may compete.
func assertPlanUsesAnyIndex(t testing.TB, plan string, indexNames ...string) {
	t.Helper()
	for _, indexName := range indexNames {
		if strings.Contains(plan, indexName) {
			return
		}
	}
	t.Fatalf("plan does not use any expected index %v:\n%s", indexNames, plan)
}

// Requires an exact named path.
func assertPlanUsesIndex(t testing.TB, plan string, indexName string) {
	t.Helper()
	if !strings.Contains(plan, indexName) {
		t.Fatalf("plan does not use index %s:\n%s", indexName, plan)
	}
}

// Rejects false-zero indexes that can turn an exact lookup into an open-set scan.
func assertPlanAvoidsIndexes(t testing.TB, plan string, indexNames ...string) {
	t.Helper()
	for _, indexName := range indexNames {
		if strings.Contains(plan, indexName) {
			t.Fatalf("plan unexpectedly uses index %s:\n%s", indexName, plan)
		}
	}
}
