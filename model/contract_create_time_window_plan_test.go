package model

// The reads of a create_time window of contracts must not need an index on
// transfer_contract.create_time: without transfer_contract_create_time a
// create_time range is a scan of the whole contract heap. Each test runs the
// real read, records the statements it sends to transfer_contract with their
// arguments, and plans each one again without that index and with sequential
// scans disabled, in a transaction that rolls back. transfer_contract may only
// be read as a range of its primary key bounded by contract_id.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// One statement a read sent, with its arguments.
type contractReadStatement struct {
	sql  string
	args []any
}

// Records the statements that name transfer_contract.
type contractReadTrace struct {
	stateLock  sync.Mutex
	statements []contractReadStatement
}

func (self *contractReadTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "transfer_contract") {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.statements = append(self.statements, contractReadStatement{sql: data.SQL, args: slices.Clone(data.Args)})
	}
	return ctx
}

// Nothing to record after a statement.
func (self *contractReadTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// The statements naming transfer_contract that call sends.
func traceContractReads(t testing.TB, ctx context.Context, call func()) []contractReadStatement {
	t.Helper()
	trace := &contractReadTrace{}
	scope, err := server.NewTestPgQueryScope(ctx, trace)
	server.Raise(err)
	func() {
		defer func() { server.Raise(scope.Close()) }()
		call()
	}()
	trace.stateLock.Lock()
	defer trace.stateLock.Unlock()
	return slices.Clone(trace.statements)
}

type contractReadPlanNode struct {
	NodeType       string                 `json:"Node Type"`
	Relation       string                 `json:"Relation Name"`
	Index          string                 `json:"Index Name"`
	IndexCondition string                 `json:"Index Cond"`
	Plans          []contractReadPlanNode `json:"Plans"`
}

// Plans the statement without the given transfer_contract indexes and without
// sequential scans, in a transaction that rolls back, and requires every read
// of transfer_contract to be a range of its primary key bounded by
// contract_id.
func requireContractIdRangePlan(t testing.TB, ctx context.Context, statement contractReadStatement, droppedIndexes ...string) {
	t.Helper()
	conn := acquireContractLifecycleTestConnection(t, ctx)
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	server.Raise(err)
	defer func() {
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	for _, index := range droppedIndexes {
		server.RaisePgResult(tx.Exec(ctx, `DROP INDEX IF EXISTS `+index))
	}
	server.RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
	var raw []byte
	server.Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+statement.sql, statement.args...).Scan(&raw))
	var plans []struct {
		Plan contractReadPlanNode `json:"Plan"`
	}
	server.Raise(json.Unmarshal(raw, &plans))
	if len(plans) != 1 {
		t.Fatalf("%d plans for one statement", len(plans))
	}
	ranged := false
	var inspect func(node contractReadPlanNode)
	inspect = func(node contractReadPlanNode) {
		bounded := node.Index == "transfer_contract_pkey" && strings.Contains(node.IndexCondition, "contract_id")
		switch {
		case node.Relation == "transfer_contract" && strings.Contains(node.NodeType, "Seq Scan"):
			t.Fatalf("without %v the statement reads the whole contract heap:\n%s\n%s", droppedIndexes, statement.sql, raw)
		case node.Relation == "transfer_contract" && strings.HasPrefix(node.NodeType, "Index") && !bounded:
			t.Fatalf("without %v the statement reads %s unbounded by contract_id:\n%s\n%s", droppedIndexes, node.Index, statement.sql, raw)
		case node.NodeType == "Bitmap Index Scan" && strings.HasPrefix(node.Index, "transfer_contract") && !bounded:
			t.Fatalf("without %v the statement reads %s unbounded by contract_id:\n%s\n%s", droppedIndexes, node.Index, statement.sql, raw)
		}
		ranged = ranged || bounded
		for _, child := range node.Plans {
			inspect(child)
		}
	}
	inspect(plans[0].Plan)
	if !ranged {
		t.Fatalf("without %v the statement reads no contract id range:\n%s\n%s", droppedIndexes, statement.sql, raw)
	}
}

// The public contract stats' live bucket count and hour bucket fill each read
// only their window's contract id range.
func TestContractStatsWindowReadsNeedNoCreateTimeIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		hour := server.NowUtc().Truncate(time.Hour)
		statements := traceContractReads(t, ctx, func() {
			countContractRange(ctx, hour, hour.Add(30*time.Minute))
			fillContractHourBuckets(ctx, hour.Add(-23*time.Hour), hour)
		})
		if len(statements) != 2 {
			t.Fatalf("the stats reads sent %d transfer_contract statements, want 2", len(statements))
		}
		for _, statement := range statements {
			requireContractIdRangePlan(t, ctx, statement, "transfer_contract_create_time")
		}
	})
}

// The degradation check's count of contracts created in its window reads only
// the window's contract id range.
func TestContractDegradationOpenCountNeedsNoCreateTimeIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		var openStatements []contractReadStatement
		statements := traceContractReads(t, ctx, func() {
			if _, _, err := MeasureContractDegradation(ctx, server.NowUtc()); err != nil {
				t.Fatal(err)
			}
		})
		for _, statement := range statements {
			// the close count reads its own partial index of terminal outcomes
			if !strings.Contains(statement.sql, "close_time") {
				openStatements = append(openStatements, statement)
			}
		}
		if len(openStatements) != 1 {
			t.Fatalf("the degradation check sent %d open-count statements, want 1", len(openStatements))
		}
		requireContractIdRangePlan(t, ctx, openStatements[0], "transfer_contract_create_time")
	})
}
