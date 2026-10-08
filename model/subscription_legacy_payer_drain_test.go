// Real financial owners prove payer scope, bounded continuation and custody.
package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Choose the routing byte before inserting any contract-dependent rows. A
// monotonic suffix prevents low-byte replacement from collapsing fixture IDs.
func legacyPayerTestContractId(prefix server.Id, sequence uint32, shard int) server.Id {
	id := prefix
	binary.BigEndian.PutUint32(id[11:15], sequence)
	id[15] = byte(shard)
	return id
}

// Seed an exact legacy reservation with a chosen identity, then let public
// CloseContract create both reports and the guarded durable settlement intent.
func newLegacyPayerTestIntent(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, id server.Id, reserved, used ByteCount) server.Id {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
 VALUES($1,$2,$3,$4,$5,$2,$6,true)`, id, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, reserved))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,$3)`, id, f.balanceId, reserved))
	})
	refreshNetEscrow(ctx, []server.Id{f.balanceId})
	server.Raise(CloseContract(ctx, id, f.sourceId, used, false))
	server.Raise(CloseContract(ctx, id, f.destinationId, used, false))
	return id
}

// All original shards share one payer turn; a different payer is untouched.
func TestLegacyPayerDrainCrossShardConservation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor, neighborId := legacySettlementTestIntent(t, ctx)
		ids := make([]server.Id, 32)
		reports := make(map[server.Id][]byte, len(ids))
		prefix := server.NewId()
		for index := range ids {
			id := legacyPayerTestContractId(prefix, uint32(index+1), index%LegacySettlementShardCount)
			ids[index] = newLegacyPayerTestIntent(t, ctx, f, id, 10, 3)
			reports[ids[index]] = legacyDrainTestReports(ctx, ids[index])
		}
		result, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, len(ids))
		if err != nil || result.Completed != len(ids) || result.Failed != 0 || result.BusyOrGone != 0 || result.FinancialCohortCompleted == 0 {
			t.Fatalf("payer turn did not complete all shards: %+v err=%v", result, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled')=32
 AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($1))
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=904
 AND (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=96
 AND (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled AND payout_byte_count=3)=32`, ids, f.balanceId).Scan(&exact))
			if !exact {
				t.Fatal("payer batch changed exact debit, payout or reservation custody")
			}
		})
		for _, id := range ids {
			if !bytes.Equal(reports[id], legacyDrainTestReports(ctx, id)) {
				t.Fatal("payer drain rewrote an original report")
			}
		}
		requireLegacySettlementTestState(t, ctx, neighbor, neighborId, true, false, 1000, 100)
		replay, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, len(ids))
		if err != nil || replay.Completed != 0 || replay.Visited != 0 {
			t.Fatal("payer replay repeated accounting", replay, err)
		}
	})
}

// A held payer cannot park another account; the original reservation and due
// key survive every busy turn and the released payer resumes exact accounting.
func TestLegacyPayerDrainHeldGrantKeepsIndependentPayerService(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		heldPayer, heldId := legacySettlementTestIntent(t, ctx)
		other, otherId := legacySettlementTestIntent(t, ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, heldPayer.balanceId))
		busy, err := FlushLegacyPayerSettlements(ctx, heldPayer.sourceNetworkId, nil, 2)
		if err != nil || busy.Completed != 0 || busy.BusyOrGone != 1 {
			t.Fatal("held payer did not yield bounded custody", busy, err)
		}
		progress, err := FlushLegacyPayerSettlements(ctx, other.sourceNetworkId, nil, 2)
		if err != nil || progress.Completed != 1 {
			t.Fatal("independent payer could not progress", progress, err)
		}
		requireLegacySettlementTestState(t, ctx, heldPayer, heldId, true, false, 1000, 100)
		requireLegacySettlementTestState(t, ctx, other, otherId, false, true, 989, 0)
		server.Raise(held.Rollback(ctx))
		progress, err = FlushLegacyPayerSettlements(ctx, heldPayer.sourceNetworkId, nil, 2)
		if err != nil || progress.Completed != 1 {
			t.Fatal("released payer was not recovered", progress, err)
		}
		requireLegacySettlementTestState(t, ctx, heldPayer, heldId, false, true, 989, 0)
	})
}

// Underfunding stays a deferred financial refusal; a future retry is not a
// reason for the task to spin, disappear or release the disputed reservation.
func TestLegacyPayerDrainPreservesAccountingCooldown(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=101 WHERE contract_id=$1`, id))
		})
		before := legacyDrainTestReports(ctx, id)
		result, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, 2)
		if err != nil || result.Failed != 1 || result.Completed != 0 {
			t.Fatal("insufficient payer work was not deferred", result, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			next, err := NextLegacySettlementPayerAttemptInTx(ctx, tx, f.sourceNetworkId)
			if err != nil || next == nil || next.Before(server.NowUtc().Add(14*time.Minute)) {
				t.Fatal("payer continuation lost accounting cooldown", next, err)
			}
		})
		if !bytes.Equal(before, legacyDrainTestReports(ctx, id)) {
			t.Fatal("refusal rewrote the original report")
		}
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
	})
}

// Custom and prepared generic plans must seek one payer under unrelated skew.
// A dense target also bounds work before LIMIT: a bitmap may otherwise collect
// its entire due history before the inner sort truncates the returned page.
func TestLegacyPayerSelectionUsesBoundedExistingIndexes(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		other := newNetEscrowOrderingTestFixture(t, ctx)
		if f.sourceNetworkId.Cmp(other.sourceNetworkId) > 0 {
			f, other = other, f
		}
		// Every empty target shard has a real successor boundary. Its earlier
		// deadline must be excluded from the finalizer's exact payer result.
		id := newLegacyPayerTestIntent(t, ctx, f, server.NewId(), 100, 11)
		seed := func(tx server.PgTx, owner netEscrowOrderingTestFixture, count int) {
			ids := make([]server.Id, count)
			prefix := server.NewId()
			for index := range ids {
				ids[index] = legacyPayerTestContractId(prefix, uint32(index+1), index%LegacySettlementShardCount)
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count)
 SELECT id,$2,$3,$4,$5,$2,0 FROM unnest($1::uuid[]) AS item(id)`, ids,
				owner.sourceNetworkId, owner.sourceId, owner.destinationNetworkId, owner.destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
 SELECT id,(get_byte(uuid_send(id),15)%16)::smallint,'settled',timestamp '2010-01-01'
 FROM unnest($1::uuid[]) AS item(id)`, ids))
		}
		server.Tx(ctx, func(tx server.PgTx) {
			seed(tx, other, 8192)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=clock_timestamp()+interval '1 day' WHERE contract_id=$1`, id))
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE legacy_settlement_intent ALTER COLUMN payer_network_id SET STATISTICS 1`))
		})
		query := legacySettlementPayerSelectionSql(`SELECT next_attempt_time,contract_id,
 statement_timestamp() AT TIME ZONE 'UTC' FROM legacy_settlement_intent
 WHERE shard=$1 AND next_attempt_time<=statement_timestamp() AT TIME ZONE 'UTC'
 ORDER BY next_attempt_time,contract_id LIMIT 8`, 8)
		for _, dense := range []bool{false, true} {
			server.Tx(ctx, func(tx server.PgTx) {
				if dense {
					seed(tx, f, 4096)
				}
				server.RaisePgResult(tx.Exec(ctx, `ANALYZE legacy_settlement_intent`))
				for _, head := range []bool{false, true} {
					selectedQuery := query
					if head {
						selectedQuery = nextLegacySettlementPayerAttemptSql
					}
					server.RaisePgResult(tx.Exec(ctx, `PREPARE payer_scope_plan(uuid) AS `+selectedQuery))
					for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
						server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode=`+mode))
						var raw []byte
						// Id.String is the canonical UUID encoding of a synthetic ID.
						server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE payer_scope_plan('`+f.sourceNetworkId.String()+`')`).Scan(&raw))
						requireLegacyPayerBoundedPlan(t, raw, mode, dense, head)
						var custom, generic int64
						server.Raise(tx.QueryRow(ctx, `SELECT custom_plans,generic_plans FROM pg_prepared_statements WHERE name='payer_scope_plan'`).Scan(&custom, &generic))
						if mode == "force_custom_plan" && custom != 1 || mode == "force_generic_plan" && (custom != 1 || generic != 1) {
							t.Fatal("plan control did not execute both prepared modes", custom, generic)
						}
					}
					server.RaisePgResult(tx.Exec(ctx, `DEALLOCATE payer_scope_plan`))
				}
				next, err := NextLegacySettlementPayerAttemptInTx(ctx, tx, f.sourceNetworkId)
				if err != nil || next == nil || dense && !next.Equal(time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)) || !dense && !next.After(server.NowUtc()) {
					t.Fatal("finalizer head lost the exact earliest attempt", dense, next, err)
				}
			})
		}
	})
}

// Count index work independently of the heap wrapper used by a bitmap plan.
// Exact predicates alone are insufficient: all examined keys must stay inside
// the sixteen capped seeks, with no discarded unrelated or target-payer rows.
func requireLegacyPayerBoundedPlan(t testing.TB, raw []byte, mode string, dense, head bool) {
	t.Helper()
	var plans []map[string]any
	server.Raise(json.Unmarshal(raw, &plans))
	root := plans[0]["Plan"].(map[string]any)
	number := func(node map[string]any, key string) float64 { value, _ := node[key].(float64); return value }
	wantRows, maxExamined, maxBuffers := float64(0), float64(0), float64(256)
	if dense {
		wantRows, maxExamined, maxBuffers = 8, 16*8, 512
	}
	if head {
		wantRows, maxExamined, maxBuffers = 1, 16, 256
	}
	if number(root, "Actual Rows") != wantRows {
		t.Fatal("payer query returned another scope or lost its bounded page", mode, dense, string(raw))
	}
	loops, examined := float64(0), float64(0)
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		if number(node, "Rows Removed by Filter") != 0 {
			children, _ := node["Plans"].([]any)
			condition, _ := node["Filter"].(string)
			boundary := head && node["Node Type"] == "Subquery Scan" && node["Alias"] == "payer_head" &&
				strings.Contains(condition, "payer_network_id =") && len(children) == 1 &&
				children[0].(map[string]any)["Node Type"] == "Limit" &&
				number(node, "Rows Removed by Filter")*number(node, "Actual Loops") <= 16
			if !boundary {
				t.Fatal("payer query filtered outside its bounded head boundary", mode, dense, string(raw))
			}
		}
		if number(node, "Rows Removed by Index Recheck") != 0 || number(node, "Lossy Heap Blocks") != 0 {
			t.Fatal("payer query filtered or lossily scanned unrelated work", mode, dense, string(raw))
		}
		if node["Relation Name"] == "legacy_settlement_intent" {
			switch node["Node Type"] {
			case "Index Scan", "Index Only Scan", "Bitmap Heap Scan":
			default:
				t.Fatal("payer query lost its indexed boundary", mode, dense, string(raw))
			}
		}
		if name, exists := node["Index Name"]; exists {
			condition, _ := node["Index Cond"].(string)
			if name != "legacy_settlement_intent_payer_due" || !strings.Contains(condition, "shard") ||
				!strings.Contains(condition, "payer_network_id") || !head && !strings.Contains(condition, "next_attempt_time") {
				t.Fatal("payer key was absent from an index seek", mode, dense, string(raw))
			}
			loops += number(node, "Actual Loops")
			examined += number(node, "Actual Rows") * number(node, "Actual Loops")
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				walk(child.(map[string]any))
			}
		}
	}
	walk(root)
	if loops != 16 || examined > maxExamined || number(root, "Shared Hit Blocks")+number(root, "Shared Read Blocks") > maxBuffers {
		t.Fatal("payer query scanned beyond its sixteen bounded seeks", mode, dense, loops, examined, string(raw))
	}
	// Retain the real successful plans as well as every failed plan for review.
	t.Logf("payer plan mode=%s dense=%t head=%t examined=%g loops=%g buffers=%g plan=%s", mode, dense, head, examined, loops,
		number(root, "Shared Hit Blocks")+number(root, "Shared Read Blocks"), raw)
}

// Every shared selector variant must retain the scope anchors, and an edited
// query shape refuses before SQL rather than silently running without its payer.
func TestLegacyPayerSelectionScopeVariantsFailClosed(t *testing.T) {
	for _, suffix := range []string{
		"", " AND (next_attempt_time,contract_id)>($2,$3)",
		" AND (next_attempt_time,contract_id)<=($2,$3)",
		" AND (next_attempt_time,contract_id)<=($2,$3) AND (next_attempt_time,contract_id)>($5,$6)",
		" AND (next_attempt_time,contract_id)<=($2,$3) AND (next_attempt_time,contract_id)>($5,$6) AND (next_attempt_time,contract_id)<=($7,$8)",
	} {
		for _, limit := range []int{1, 3, 8} {
			query := `SELECT next_attempt_time,contract_id,statement_timestamp() FROM legacy_settlement_intent
 WHERE shard=$1 AND next_attempt_time<=statement_timestamp()` + suffix + ` ORDER BY next_attempt_time,contract_id LIMIT ` + fmt.Sprint(limit)
			scoped := legacySettlementPayerSelectionSql(query, limit)
			if strings.Contains(scoped, "WHERE shard=$1") || strings.Count(scoped, "payer_network_id=$1::uuid") != 1 ||
				strings.Count(scoped, "payer_network_id IS NOT NULL") != 1 ||
				strings.Contains(scoped, "payer_network_id>=") || strings.Contains(scoped, "payer_network_id<=") ||
				!strings.Contains(scoped, "LIMIT "+fmt.Sprint(limit)+") AS payer_page") {
				t.Fatal("selection variant lost bounded payer scope", scoped)
			}
		}
	}
	for _, query := range []string{
		"SELECT 1", "SELECT contract_id FROM legacy_settlement_intent WHERE shard=$1 LIMIT 1",
		"SELECT next_attempt_time,contract_id,statement_timestamp() FROM legacy_settlement_intent WHERE shard=$1 ORDER BY next_attempt_time,contract_id LIMIT 16",
		"SELECT next_attempt_time,contract_id,statement_timestamp() FROM legacy_settlement_intent WHERE shard=$1 ORDER BY next_attempt_time,contract_id LIMIT 128",
		"SELECT next_attempt_time,contract_id,statement_timestamp() FROM legacy_settlement_intent WHERE shard=$1 ORDER BY next_attempt_time,contract_id LIMIT 1 OFFSET 1",
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("changed selector shape did not fail closed")
				}
			}()
			legacySettlementPayerSelectionSql(query, 1)
		}()
	}
}
