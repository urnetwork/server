package model

// The index diet's plan proofs (DBDISK-STABILIZE §S3, 2026-11-16). Each test
// seeds contracts that make the wrong path visibly expensive, plans the real
// statement with sequential scans disabled in a transaction that rolls back,
// and walks the JSON plan:
//   - prober retention's classification reads only its batch's ids from the
//     primary key once transfer_contract_open_payer_network_id_transfer_byte_count
//     is gone;
//   - the force-close deadline pages read their own partial indexes once
//     transfer_contract_create_time is gone;
//   - the companion lingering-origin arm reads closed contracts of one pair
//     as a range of transfer_contract_open_source_id, which an index of open
//     rows only cannot replace.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// One node of an EXPLAIN (FORMAT JSON) plan.
type indexDietPlanNode struct {
	NodeType         string              `json:"Node Type"`
	Relation         string              `json:"Relation Name"`
	Index            string              `json:"Index Name"`
	IndexCondition   string              `json:"Index Cond"`
	RecheckCondition string              `json:"Recheck Cond"`
	Filter           string              `json:"Filter"`
	Plans            []indexDietPlanNode `json:"Plans"`
}

// One index a transfer_contract scan reads, with its condition.
type indexDietIndexRead struct {
	index     string
	condition string
}

// One scan of transfer_contract: the indexes it reads (a bitmap heap scan reads
// every bitmap index scan below it) and every condition it applies.
type indexDietContractScan struct {
	nodeType   string
	reads      []indexDietIndexRead
	conditions string
}

// The bitmap index scans below a bitmap heap scan, through BitmapAnd and
// BitmapOr.
func indexDietBitmapReads(node indexDietPlanNode) []indexDietIndexRead {
	if node.NodeType == "Bitmap Index Scan" {
		return []indexDietIndexRead{{index: node.Index, condition: node.IndexCondition}}
	}
	reads := []indexDietIndexRead{}
	for _, child := range node.Plans {
		reads = append(reads, indexDietBitmapReads(child)...)
	}
	return reads
}

// Every scan of transfer_contract in the plan.
func indexDietContractScans(node indexDietPlanNode) []indexDietContractScan {
	scans := []indexDietContractScan{}
	if node.Relation == "transfer_contract" {
		scan := indexDietContractScan{
			nodeType:   node.NodeType,
			conditions: strings.Join([]string{node.IndexCondition, node.RecheckCondition, node.Filter}, " "),
		}
		switch {
		case node.NodeType == "Bitmap Heap Scan":
			for _, child := range node.Plans {
				scan.reads = append(scan.reads, indexDietBitmapReads(child)...)
			}
		case node.Index != "":
			scan.reads = []indexDietIndexRead{{index: node.Index, condition: node.IndexCondition}}
		}
		for _, read := range scan.reads {
			scan.conditions += " " + read.condition
		}
		scans = append(scans, scan)
	}
	for _, child := range node.Plans {
		scans = append(scans, indexDietContractScans(child)...)
	}
	return scans
}

// Plans the statement in a transaction that rolls back: after the setup
// statements (dropped or replaced indexes, planner settings) and with
// sequential scans disabled. Returns the scans of transfer_contract and the
// raw plan.
func planIndexDietStatement(
	t testing.TB,
	ctx context.Context,
	setup []string,
	sql string,
	args ...any,
) ([]indexDietContractScan, string) {
	t.Helper()
	conn := acquireContractLifecycleTestConnection(t, ctx)
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	server.Raise(err)
	defer func() {
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	for _, statement := range append(setup, `SET LOCAL enable_seqscan = off`) {
		server.RaisePgResult(tx.Exec(ctx, statement))
	}
	var raw []byte
	server.Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+sql, args...).Scan(&raw))
	var plans []struct {
		Plan indexDietPlanNode `json:"Plan"`
	}
	server.Raise(json.Unmarshal(raw, &plans))
	if len(plans) != 1 {
		t.Fatalf("%d plans for one statement", len(plans))
	}
	return indexDietContractScans(plans[0].Plan), string(raw)
}

// Inserts count contracts closed a second after their creation, created
// 100 ms apart going back from end, each id minted at its creation millisecond.
// A nil source or destination is a new random client per contract.
func insertIndexDietClosedContracts(
	t testing.TB,
	ctx context.Context,
	sourceId *server.Id,
	destinationId *server.Id,
	payerNetworkId server.Id,
	end time.Time,
	count int,
) {
	t.Helper()
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id, destination_network_id, destination_id,
				transfer_byte_count, create_time, close_time, payer_network_id, outcome
			)
			SELECT
				(lpad(to_hex(floor(extract(epoch FROM c.create_time) * 1000)::bigint), 12, '0') ||
					substr(md5(n::text || ':' || gen_random_uuid()::text), 1, 20))::uuid,
				gen_random_uuid(), COALESCE($1::uuid, gen_random_uuid()),
				gen_random_uuid(), COALESCE($2::uuid, gen_random_uuid()),
				1, c.create_time, c.create_time + interval '1 second', $3, 'success'
			FROM generate_series(1, $5::integer) AS n
			CROSS JOIN LATERAL (
				SELECT $4::timestamp - n * interval '100 milliseconds' AS create_time
			) AS c
			`,
			sourceId,
			destinationId,
			payerNetworkId,
			end.UTC(),
			count,
		))
	})
}

func analyzeIndexDietContracts(t testing.TB, ctx context.Context) {
	t.Helper()
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract`))
	}, server.OptReadWrite(), server.OptNoRetry())
}

// The prober shard retention classifies a batch named by contract id with the
// bitmap settings it runs under (contractHorizonBitmapSettings, custom plans).
// Main (11-16) still read transfer_contract_open_payer_network_id_transfer_byte_count
// from it, about 3.5 bitmap index scans a call over the payer's history, ANDed
// with the primary key's bitmap. Without that index every read of
// transfer_contract must be the batch's ids from the primary key, never the
// payer's history or the heap.
func TestProberRetentionClassifyNeedsNoOpenPayerIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		payerNetworkId := server.NewId()
		// the shard payer's closed history, and as much of another payer's
		insertIndexDietClosedContracts(t, ctx, nil, nil, payerNetworkId, now, 20000)
		insertIndexDietClosedContracts(t, ctx, nil, nil, server.NewId(), now, 20000)
		analyzeIndexDietContracts(t, ctx)

		var windowIds []server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT ARRAY(SELECT contract_id FROM transfer_contract WHERE payer_network_id = $1 ORDER BY contract_id LIMIT 100)`,
				payerNetworkId,
			).Scan(&windowIds))
		})
		if len(windowIds) != 100 {
			t.Fatalf("window holds %d contracts, want 100", len(windowIds))
		}

		// the batch's payers are a generation's pass networks: about 1,600 a
		// day on Main, the shard payer among them
		networkIds := []server.Id{payerNetworkId}
		for range 1599 {
			networkIds = append(networkIds, server.NewId())
		}

		setup := append([]string{
			`DROP INDEX IF EXISTS transfer_contract_open_payer_network_id_transfer_byte_count`,
			`SET LOCAL plan_cache_mode = force_custom_plan`,
		}, proberShardRetentionBitmapSettings...)
		for _, sql := range []string{proberShardRetentionClassifySql, proberShardRetentionClassifyLockSql} {
			scans, raw := planIndexDietStatement(t, ctx, setup, sql, windowIds, networkIds, now, ContractPartyDestination)
			if len(scans) == 0 {
				t.Fatalf("the classification reads no transfer_contract:\n%s", raw)
			}
			for _, scan := range scans {
				if len(scan.reads) == 0 {
					t.Fatalf("without the open payer index the classification reads transfer_contract by %s:\n%s", scan.nodeType, raw)
				}
				for _, read := range scan.reads {
					if read.index != "transfer_contract_pkey" || !strings.Contains(read.condition, "contract_id") {
						t.Fatalf("without the open payer index the classification reads %s (%s), not the batch's ids:\n%s", read.index, read.condition, raw)
					}
				}
			}
		}
	})
}

// The force-close deadline pages, the live readers of a create_time range
// (Main 11-16: the open page about 17 a second, the disputed page about 4),
// each read their own partial index in create_time order. Without
// transfer_contract_create_time they must keep reading it, never the heap, the
// primary key or another index.
func TestCloseDeadlinePagesNeedNoCreateTimeIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		insertIndexDietClosedContracts(t, ctx, nil, nil, server.NewId(), now, 20000)
		analyzeIndexDietContracts(t, ctx)

		setup := []string{`DROP INDEX IF EXISTS transfer_contract_create_time`}
		cursorTime := now.Add(-2 * time.Hour)
		cursorId := server.Id{}
		for _, page := range []struct {
			sql   string
			args  []any
			index string
		}{
			{
				sql:   forceCloseOpenContractPageSql,
				args:  []any{ContractPartySource, ContractPartyDestination, now.Add(-12 * time.Minute), 1000, cursorTime, cursorId, now},
				index: "transfer_contract_open_partial_create_time",
			},
			{
				sql:   forceCloseDisputedContractPageSql,
				args:  []any{now.Add(-12 * time.Minute), 1000, cursorTime, cursorId, now},
				index: "transfer_contract_dispute_partial_create_time",
			},
		} {
			scans, raw := planIndexDietStatement(t, ctx, setup, page.sql, page.args...)
			if len(scans) != 1 {
				t.Fatalf("the page reads transfer_contract %d times, want 1:\n%s", len(scans), raw)
			}
			reads := scans[0].reads
			if len(reads) != 1 || reads[0].index != page.index {
				t.Fatalf("without transfer_contract_create_time the page reads %v, want %s:\n%s", reads, page.index, raw)
			}
		}
	})
}

// The companion lingering-origin arm: a closed origin anchors companions until
// its deadline (CompanionOriginCloseLinger), so the origin reads ask for closed
// contracts of one pair, bounded by the unexpired contract id floor. On Main
// (11-16) that is about 1,200 scans a second of transfer_contract_open_source_id
// (open, source_id, destination_id, contract_id). DBDISK-STABILIZE §S3 asked to
// shrink that index to a partial index of open rows; this pins why it cannot
// replace the full one while the arm reads closed rows. With the full index the
// arm is a pair range; with the open-rows partial in its place the arm loses
// every pair condition and reads the destination's whole recent fan-in or every
// recent contract instead.
func TestCompanionClosedOriginReadsNeedTheClosedPairIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		if server.IsContractPartitioned(t.Context()) {
			t.Skip("plain contract tables only")
		}
		runtimeQueries := coveredCompanionRuntimeQueries(t)
		ctx := t.Context()
		now := server.NowUtc()
		sourceId := server.NewId()
		destinationId := server.NewId()
		payerNetworkId := server.NewId()
		// the pair's own recent closed origins, the destination's fan-in from
		// other sources, and recent closed contracts of other pairs
		insertIndexDietClosedContracts(t, ctx, &sourceId, &destinationId, payerNetworkId, now, 50)
		insertIndexDietClosedContracts(t, ctx, nil, &destinationId, server.NewId(), now, 20000)
		insertIndexDietClosedContracts(t, ctx, nil, nil, server.NewId(), now, 20000)
		analyzeIndexDietContracts(t, ctx)

		closeFloor := now.Add(-CompanionOriginCloseLinger)
		originArgs := []any{sourceId, destinationId, closeFloor, payerNetworkId}
		if runtimeQueries["earliest_origin"].binds == 5 {
			originArgs = append(originArgs, ByteCount(1024))
		}
		statements := []struct {
			name string
			sql  string
			args []any
		}{
			{name: "companion origin exists", sql: companionOriginExistsSql, args: []any{sourceId, destinationId, closeFloor}},
			{name: "earliest origin", sql: runtimeQueries["earliest_origin"].sql, args: originArgs},
		}

		// the closed arm's scans: the ones that test the close time
		closedArmScans := func(scans []indexDietContractScan) []indexDietContractScan {
			closed := []indexDietContractScan{}
			for _, scan := range scans {
				if strings.Contains(scan.conditions, "close_time") {
					closed = append(closed, scan)
				}
			}
			return closed
		}
		pairRange := func(read indexDietIndexRead) bool {
			return strings.Contains(read.condition, "source_id") && strings.Contains(read.condition, "destination_id")
		}

		for _, statement := range statements {
			scans, raw := planIndexDietStatement(t, ctx, nil, statement.sql, statement.args...)
			closed := closedArmScans(scans)
			if len(closed) == 0 {
				t.Fatalf("%s: no scan tests the close time:\n%s", statement.name, raw)
			}
			for _, scan := range closed {
				if len(scan.reads) == 0 || scan.reads[0].index != "transfer_contract_open_source_id" || !pairRange(scan.reads[0]) {
					t.Fatalf("%s: the closed arm reads %v, want a pair range of transfer_contract_open_source_id:\n%s", statement.name, scan.reads, raw)
				}
			}
		}

		// the S3 shape: open rows only, in place of the full index
		openRowsOnly := []string{
			`DROP INDEX transfer_contract_open_source_id`,
			`CREATE INDEX transfer_contract_open_source_id_open_rows ON transfer_contract (source_id, destination_id, contract_id) WHERE open`,
		}
		for _, statement := range statements {
			scans, raw := planIndexDietStatement(t, ctx, openRowsOnly, statement.sql, statement.args...)
			closed := closedArmScans(scans)
			if len(closed) == 0 {
				t.Fatalf("%s: no scan tests the close time:\n%s", statement.name, raw)
			}
			for _, scan := range closed {
				for _, read := range scan.reads {
					if read.index == "transfer_contract_open_source_id_open_rows" || pairRange(read) {
						t.Fatalf("%s: the closed arm still reads a pair range (%s), so an open-rows index could replace the full one:\n%s", statement.name, read.index, raw)
					}
				}
			}
		}
	})
}
