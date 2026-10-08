// Finite real owners establish accounting, prefix and transaction amortization.
package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

type legacyFinancialCohortFixture struct {
	payer              netEscrowOrderingTestFixture
	providers          []netEscrowOrderingTestFixture
	ids                []server.Id
	balances           []server.Id
	balanceIds         []server.Id
	providerIndexes    []int
	reservationAmounts []int64
	shards             []int
	reports            []byte
}

// Two grants belong to the same payer, with a 15:1 distribution. Four provider
// networks keep exact individual payout owners. Setup alone writes synthetic
// retained legacy state; measured settlement uses the ordinary public owner.
func legacyFinancialCohortSeed(t testing.TB, ctx context.Context, count int, shardCounts ...int) legacyFinancialCohortFixture {
	t.Helper()
	f := legacyFinancialCohortFixture{payer: newNetEscrowOrderingTestFixture(t, ctx)}
	for range 4 {
		f.providers = append(f.providers, newNetEscrowOrderingTestFixture(t, ctx))
	}
	f.balances = []server.Id{f.payer.balanceId, server.NewId()}
	shardCount := 1
	if len(shardCounts) > 0 {
		shardCount = shardCounts[0]
	}
	prefix := server.NewId()
	destinationIds := make([]server.Id, count)
	destinationNetworks := make([]server.Id, count)
	for index := range count {
		id := prefix
		binary.BigEndian.PutUint32(id[11:15], uint32(index+1))
		shard := 1
		if shardCount > 1 {
			shard = index % shardCount
		}
		id[15] = byte(shard)
		f.shards = append(f.shards, shard)
		f.ids = append(f.ids, id)
		balance := 0
		if (index/shardCount)%16 == 15 {
			balance = 1
		}
		f.balanceIds = append(f.balanceIds, f.balances[balance])
		f.providerIndexes = append(f.providerIndexes, (index/shardCount)%4)
		f.reservationAmounts = append(f.reservationAmounts, 16)
		destinationIds[index] = f.providers[(index/shardCount)%4].destinationId
		destinationNetworks[index] = f.providers[(index/shardCount)%4].destinationNetworkId
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=1000000,
            balance_byte_count=1000000,net_revenue_nano_cents=2000000 WHERE balance_id=$1`, f.payer.balanceId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance(balance_id,network_id,start_time,end_time,
            start_balance_byte_count,balance_byte_count,net_revenue_nano_cents)
            VALUES($1,$2,$3,$4,1000000,1000000,2000000)`, f.balances[1], f.payer.sourceNetworkId, server.NowUtc(), server.NowUtc().Add(2*time.Hour)))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,
            destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
            SELECT id,$2,$3,network_id,client_id,$2,16,true
            FROM unnest($1::uuid[],$4::uuid[],$5::uuid[]) AS row(id,network_id,client_id)`,
			f.ids, f.payer.sourceNetworkId, f.payer.sourceId, destinationNetworks, destinationIds))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
            SELECT id,balance,16 FROM unnest($1::uuid[],$2::uuid[]) AS row(id,balance)`, f.ids, f.balanceIds))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
            SELECT id,party,3,'2010-01-01'::timestamp,false FROM unnest($1::uuid[]) AS row(id)
            CROSS JOIN (VALUES ('source'),('destination')) AS parties(party)`, f.ids))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,clear_dispute,next_attempt_time,payer_network_id)
            SELECT id,shard,'settled',false,'2010-01-01'::timestamp,$2 FROM unnest($1::uuid[],$3::smallint[]) AS row(id,shard)`, f.ids, f.payer.sourceNetworkId, f.shards))
	}, server.TxReadCommitted, server.OptNoRetry())
	refreshNetEscrow(ctx, f.balances)
	f.reports = legacyFinancialCohortReports(ctx, f.ids)
	return f
}

func legacyFinancialCohortReports(ctx context.Context, ids []server.Id) []byte {
	var reports []byte
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(
        contract_id,party,used_transfer_byte_count,checkpoint,close_time) ORDER BY contract_id,party)
        FROM contract_close WHERE contract_id=ANY($1)`, ids).Scan(&reports))
	})
	return reports
}

// Checks exact custody and durable provider allocations without executing any
// payout repair. The original reports and raw per-contract usage are independent
// oracles, not a recomputation through the candidate's planning function.
func legacyFinancialCohortRequire(t testing.TB, ctx context.Context, f legacyFinancialCohortFixture, completed map[server.Id]bool) {
	t.Helper()
	if !bytes.Equal(f.reports, legacyFinancialCohortReports(ctx, f.ids)) {
		t.Fatal("cohort changed original reports")
	}
	debits := map[server.Id]int64{}
	providerBytes := map[server.Id]int64{}
	for index, id := range f.ids {
		if completed[id] {
			debits[f.balanceIds[index]] += 3
			providerBytes[f.providers[f.providerIndexes[index]].destinationNetworkId] += 3
		}
	}
	server.Db(ctx, func(conn server.PgConn) {
		for index, id := range f.ids {
			var outcome *string
			var pending, settled bool
			var escrowAmount, payout, revenue int64
			var usage []byte
			server.Raise(conn.QueryRow(ctx, `SELECT c.outcome,c.provider_usage,
                EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=c.contract_id),e.settled,e.balance_byte_count,
                COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=c.contract_id),0),
                COALESCE((SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=c.contract_id),0)
                FROM transfer_contract c JOIN transfer_escrow e USING(contract_id) WHERE c.contract_id=$1 AND e.balance_id=$2`, id, f.balanceIds[index]).Scan(
				&outcome, &usage, &pending, &settled, &escrowAmount, &payout, &revenue))
			if escrowAmount != f.reservationAmounts[index] {
				t.Fatal("cohort changed immutable reservation capacity", index, escrowAmount)
			}
			if completed[id] {
				if outcome == nil || *outcome != "settled" || pending || !settled || payout != 3 || revenue != 3 {
					t.Fatal("cohort lost an exact completed financial owner", index)
				}
				record, err := decodeContractUsageSnapshot(usage)
				provider := f.providers[f.providerIndexes[index]]
				if err != nil || record.ByteCount != 3 || len(record.Providers) != 1 || record.Providers[0].ClientId != provider.destinationId || record.Providers[0].NetworkId != provider.destinationNetworkId || record.Providers[0].ByteCount != 3 {
					t.Fatal("cohort changed original usage attribution", index, err)
				}
			} else if outcome != nil || !pending || settled || payout != 0 || revenue != 0 || len(usage) != 0 {
				t.Fatal("cohort wrote an uncompleted financial owner", index)
			}
		}
		for _, balanceId := range f.balances {
			var credit int64
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, balanceId).Scan(&credit))
			if credit != 1000000-debits[balanceId] {
				t.Fatal("cohort payer debit differs", credit, debits[balanceId])
			}
		}
		for _, provider := range f.providers {
			var amount, revenue int64
			server.Raise(conn.QueryRow(ctx, `SELECT
                COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$1),0)+
                COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM pending_task
                    CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
                    WHERE function_name=$2 AND NOT (args_json::jsonb->>'applied')::boolean AND (allocation->>'network_id')::uuid=$1),0),
                COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$1),0)+
                COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM pending_task
                    CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
                    WHERE function_name=$2 AND NOT (args_json::jsonb->>'applied')::boolean AND (allocation->>'network_id')::uuid=$1),0)`,
				provider.destinationNetworkId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(&amount, &revenue))
			if amount != providerBytes[provider.destinationNetworkId] || revenue != amount {
				t.Fatal("cohort lost durable provider allocation", amount, revenue)
			}
		}
	})
}

func legacyFinancialCohortCompleted(ids []server.Id) map[server.Id]bool {
	out := map[server.Id]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// The full public path proves eight financial outcomes share one commit. On
// the unmodified public owner the money oracles pass and the final count is
// eight, giving the causal red without asserting machine-dependent wall time.
func TestLegacyFinancialCohortTransactionAmortization(t *testing.T) {
	if os.Getenv("URN_LEGACY_REQUIRE_PGSS") != "1" {
		t.Skip("isolated statement statistics required")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`))
		})
		f := legacyFinancialCohortSeed(t, ctx, 8)
		beforeCounter := contractClosedCounter.Snapshot()
		before := legacyTargetSqlSnapshot(t, ctx)
		page, err := FlushLegacySettlements(ctx, 1, nil, 8)
		after := legacyTargetSqlSnapshot(t, ctx)
		afterCounter := contractClosedCounter.Snapshot()
		if err != nil || page.Visited != 8 || page.Completed != 8 || page.Failed != 0 || page.BusyOrGone != 0 {
			t.Fatal("public cohort did not complete", page, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		if !beforeCounter.Stable || !afterCounter.Stable || afterCounter.Confirmed-beforeCounter.Confirmed != 8 || afterCounter.Uncertain != beforeCounter.Uncertain || afterCounter.Untracked != beforeCounter.Untracked {
			t.Fatal("cohort changed per-contract confirmed commit counting", beforeCounter, afterCounter)
		}
		if replay, err := FlushLegacySettlements(ctx, 1, nil, 8); err != nil || replay.Completed != 0 || replay.Visited != 0 {
			t.Fatal("cohort replay changed financial state", replay, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		delta := legacyTargetSqlDelta(t, before, after)
		commits := float64(0)
		for _, statement := range delta.Statements {
			if statement.TopLevel && strings.EqualFold(strings.TrimSpace(statement.Query), "commit") {
				commits += statement.Metrics["calls"]
			}
		}
		raw, _ := json.Marshal(delta)
		t.Logf("cohort_commit_work contracts=8 commits=%g page=%+v sql=%s", commits, page, raw)
		if commits != 1 {
			t.Fatalf("eight healthy closes still require %g commits; expected one bounded cohort", commits)
		}
	})
}

// One malformed own reservation cannot be funded by its healthy siblings.
// The first three rows commit, the refusal gets ordinary backoff, and later
// rows remain untouched until another public call. No grant/report repair runs.
func TestLegacyFinancialCohortInsufficientEscrowPreservesPrefix(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacyFinancialCohortSeed(t, ctx, 8)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=2 WHERE contract_id=$1`, f.ids[3]))
		}, server.TxReadCommitted)
		f.reservationAmounts[3] = 2
		page, err := FlushLegacySettlements(ctx, 1, nil, 8)
		if err != nil || page.Visited != 4 || page.Completed != 3 || page.Failed != 1 || page.Cursor == nil || page.Cursor.ContractId != f.ids[3] {
			t.Fatal("invalid row changed the committed chronological prefix", page, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids[:3]))
		var deferred bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT failure_code='accounting' AND next_attempt_time>statement_timestamp() AT TIME ZONE 'UTC'
            FROM legacy_settlement_intent WHERE contract_id=$1`, f.ids[3]).Scan(&deferred))
		})
		if !deferred {
			t.Fatal("accounting refusal lost durable backoff")
		}
		resumed, err := FlushLegacySettlements(ctx, 1, page.Cursor, 8)
		if err != nil || resumed.Completed != 4 || resumed.Failed != 0 {
			t.Fatal("healthy tail did not resume", resumed, err)
		}
		expected := legacyFinancialCohortCompleted(f.ids)
		delete(expected, f.ids[3])
		legacyFinancialCohortRequire(t, ctx, f, expected)
	})
}

// The real grant owner fixes a deterministic busy subset, while other grants
// in the same cohort still complete. Release changes ownership alone.
func TestLegacyFinancialCohortSkipsHeldGrantIndividually(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacyFinancialCohortSeed(t, ctx, 32)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.balances[0]))
		page, err := FlushLegacySettlements(ctx, 1, nil, 32)
		if err != nil || page.Visited != 32 || page.Completed != 2 || page.BusyGrantSetMismatch != 30 || page.Failed != 0 {
			t.Fatal("held grant blocked a separately owned grant in its cohort", page, err)
		}
		expected := map[server.Id]bool{f.ids[15]: true, f.ids[31]: true}
		legacyFinancialCohortRequire(t, ctx, f, expected)
		server.Raise(held.Rollback(ctx))
		released, err := FlushLegacySettlements(ctx, 1, nil, 32)
		if err != nil || released.Completed != 30 || released.BusyOrGone != 0 || released.Failed != 0 {
			t.Fatal("released grant failed ordinary progress", released, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
	})
}

// A trigger failure after the guarded outcomes forces actual transaction
// rollback, including earlier cohort members and required durable owners.
func TestLegacyFinancialCohortRollbackRetainsEveryOwner(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacyFinancialCohortSeed(t, ctx, 8)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION cohort_refuse_metadata() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN IF NEW.settled THEN RAISE EXCEPTION 'synthetic cohort metadata refusal'; END IF; RETURN NEW; END $$`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER cohort_refuse_metadata BEFORE UPDATE ON transfer_escrow FOR EACH ROW EXECUTE FUNCTION cohort_refuse_metadata()`))
		})
		before := contractClosedCounter.Snapshot()
		attempts, err := flushLegacySettlementCohort(ctx, f.ids)
		after := contractClosedCounter.Snapshot()
		if err != nil || len(attempts) != 1 || !attempts[0].fallback || before.Confirmed != after.Confirmed {
			t.Fatal("cohort treated a rolled-back outcome as committed", attempts, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, map[server.Id]bool{})
		if legacyTargetMirrorQueueCount(ctx) != 0 {
			t.Fatal("rollback retained a mirror producer")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER cohort_refuse_metadata ON transfer_escrow`))
		})
		page, err := FlushLegacySettlements(ctx, 1, nil, 8)
		if err != nil || page.Completed != 8 {
			t.Fatal("rollback prevented unchanged ordinary retry", page, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
	})
}

// A later tuple owner makes the cohort roll back before commit. Fallback must
// then commit the healthy first contract and attach the operational backoff to
// the actually held second contract, rather than penalizing the first cursor.
func TestLegacyFinancialCohortHeldEscrowAttributesFallback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacyFinancialCohortSeed(t, ctx, 8)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_escrow WHERE contract_id=$1 FOR UPDATE`, f.ids[1]))
		page, err := FlushLegacySettlements(ctx, 1, nil, 8)
		if err != nil || page.Visited != 2 || page.Completed != 1 || page.Failed != 1 || page.Cursor == nil || page.Cursor.ContractId != f.ids[1] {
			t.Fatal("cohort attributed a later lock failure to a healthy earlier row", page, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids[:1]))
		var actualOwner bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT failure_code='operational' AND next_attempt_time>statement_timestamp() AT TIME ZONE 'UTC'
            FROM legacy_settlement_intent WHERE contract_id=$1`, f.ids[1]).Scan(&actualOwner))
		})
		if !actualOwner {
			t.Fatal("held tuple lost its own durable backoff")
		}
		server.Raise(held.Rollback(ctx))
		// Direct ordinary ownership does not wait for scheduler eligibility;
		// releasing the real tuple lock is the only financial-state change.
		for _, id := range f.ids[1:] {
			completed, busy, _, err := flushLegacySettlement(ctx, id)
			if err != nil || !completed || busy {
				t.Fatal("released tuple or its untouched tail could not settle", err)
			}
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
	})
}
