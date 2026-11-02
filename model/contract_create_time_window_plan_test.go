package model

// The reads of a create_time window of contracts must not need an index on
// transfer_contract.create_time: without transfer_contract_create_time a
// create_time range is a scan of the whole contract heap, or a skip scan of
// transfer_contract_destination_id_create_time once per destination. Each test
// seeds decoy contracts that make that skip scan the planner's cheapest path
// for any read whose create_time window is an index condition, runs the real
// read, records the statements it sends to transfer_contract with their
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

// The margin ContractIdCreateTimeRange allows between a contract's id and its
// create_time, written out so these tests also build against the reads that
// came before it.
const contractReadTestMargin = 15 * time.Minute

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

// Contracts created in the margin before start, inside the id range of a
// window from start but outside the window, all from one source to one
// destination, closed as old contracts are, and analyzed. The id range must
// read past them, while a skip scan of
// transfer_contract_destination_id_create_time descends once for the one
// destination and reads only the window.
func seedContractIdRangeDecoys(t testing.TB, ctx context.Context, start time.Time) {
	t.Helper()
	insertClosedContractsBefore(t, ctx, start, 2000)
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract`))
	}, server.OptReadWrite(), server.OptNoRetry())
}

// Inserts count closed contracts created in the margin before start, from one
// source to one destination, each id minted at its creation.
func insertClosedContractsBefore(t testing.TB, ctx context.Context, start time.Time, count int) {
	t.Helper()
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id, destination_network_id, destination_id,
				transfer_byte_count, create_time, close_time
			)
			SELECT
				(lpad(to_hex(floor(extract(epoch FROM decoy.create_time) * 1000)::bigint), 12, '0') || substr(md5(n::text), 1, 20))::uuid,
				$2, $3, $4, $5, 0, decoy.create_time, decoy.create_time
			FROM generate_series(1, $6::integer) AS n
			CROSS JOIN LATERAL (
				SELECT $1::timestamp - n * ($7::bigint * interval '1 millisecond') AS create_time
			) AS decoy
			`,
			start.UTC(),
			server.NewId(),
			server.NewId(),
			server.NewId(),
			server.NewId(),
			count,
			(contractReadTestMargin / time.Duration(count+1)).Milliseconds(),
		))
	})
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
		seedContractIdRangeDecoys(t, ctx, hour)
		seedContractIdRangeDecoys(t, ctx, hour.Add(-23*time.Hour))
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
		now := server.NowUtc()
		seedContractIdRangeDecoys(t, ctx, now.Add(-ContractDegradationWindow))
		var openStatements []contractReadStatement
		statements := traceContractReads(t, ctx, func() {
			if _, _, err := MeasureContractDegradation(ctx, now); err != nil {
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

// The straggler stamp's batch reads only the contract ids of the contracts
// created before its horizon, with neither create_time index.
func TestStragglerStampNeedsNoCreateTimeIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		horizon := now.Add(-StragglerContractExpiration)
		// Nearly every contract is closed, and nearly all of them are newer
		// than the horizon's id bound: the closed contracts of the last
		// minutes stand for them, so reading every closed contract costs more
		// than the id range. The decoys are created after the horizon, inside
		// its id bound.
		insertClosedContractsBefore(t, ctx, now, 10000)
		seedContractIdRangeDecoys(t, ctx, horizon.Add(contractReadTestMargin))
		statements := traceContractReads(t, ctx, func() {
			assignStragglerReapTimeBatches(ctx, horizon, 1000)
		})
		if len(statements) != 1 {
			t.Fatalf("the straggler stamp sent %d transfer_contract statements, want 1", len(statements))
		}
		requireContractIdRangePlan(t, ctx, statements[0], "transfer_contract_reap_pending_create_time", "transfer_contract_create_time")
	})
}
