package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// This is a selection-theory control, not an all-financial throughput benchmark.
// Each target has exactly 8,193 indexed due predecessors in its own head/forward
// interval. Predecessor callbacks delete synthetic intents only; the two tagged
// targets retain real reports, escrow, proof preparation, settlement and posts.
// Removing only predecessor intents is the causal intervention. It must make
// the same untouched targets settle without altering their financial inputs.
func TestLegacySettlementTargetDensity8193Page64(t *testing.T) {
	testLegacySettlementTargetDensity8193(t, 64)
}

func TestLegacySettlementTargetDensity8193VirtualBudget(t *testing.T) {
	testLegacySettlementTargetDensity8193(t, 256)
}

func testLegacySettlementTargetDensity8193(t *testing.T, limit int) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const predecessors = 8193
		prefix := server.NewId()
		identity := func(sequence uint32) server.Id {
			id := prefix
			binary.BigEndian.PutUint32(id[11:15], sequence)
			id[15] = 1
			return id
		}
		oldest := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
		targets := []server.Id{identity(100000), identity(200000)}
		targetTimes := []time.Time{oldest.Add(8194 * time.Millisecond), oldest.Add(time.Minute + 8194*time.Millisecond)}
		ids := append([]server.Id{}, targets...)
		times := append([]time.Time{}, targetTimes...)
		prefixIds := make([]server.Id, 0, 2*predecessors)
		for lane := range 2 {
			for index := range predecessors {
				id := identity(uint32(1 + lane*10000 + index))
				prefixIds = append(prefixIds, id)
				ids = append(ids, id)
				times = append(times, oldest.Add(time.Duration(lane)*time.Minute+time.Duration(index+1)*time.Millisecond))
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
						(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
						SELECT id,$2,$3,$4,$5,$2,CASE WHEN id=ANY($6) THEN 2 ELSE 0 END,true
						FROM unnest($1::uuid[]) AS row(id)`, ids, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, targets))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
						SELECT id,$2,2 FROM unnest($1::uuid[]) AS row(id)`, targets, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
						SELECT id,party,1,statement_timestamp() AT TIME ZONE 'UTC',false FROM unnest($1::uuid[]) AS row(id)
						CROSS JOIN (VALUES ('source'),('destination')) AS parties(party)`, targets))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,clear_dispute,next_attempt_time)
						SELECT id,1,'settled',false,due FROM unnest($1::uuid[],$2::timestamp[]) AS row(id,due)`, ids, times))
		})
		refreshNetEscrow(ctx, []server.Id{f.balanceId})
		cursor := &LegacySettlementCursor{NextAttemptTime: oldest.Add(30 * time.Second), ContractId: identity(300000), PassEndTime: oldest.Add(time.Hour)}
		for lane := range 2 {
			lower := oldest
			if lane == 1 {
				lower = cursor.NextAttemptTime
			}
			legacyTargetDensityPlan(t, ctx, lower, targetTimes[lane], targets[lane], predecessors)
		}
		assertTargets := func(settled bool) {
			t.Helper()
			want := 0
			if settled {
				want = 2
			}
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT
							(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled')=$3
							AND (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1) AND failure_code='none')=2-$3
							AND (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled)=$3
							AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=1000-$3
							AND (SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$3
							AND (SELECT COALESCE(sum(payout_net_revenue_nano_cents),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$3`, targets, f.balanceId, want).Scan(&exact))
				if !exact {
					t.Fatal("tagged target debit, intent or sweep conservation failed", settled)
				}
			})
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != int64(2*(2-want)) {
				t.Fatal("tagged target reservation changed outside ordinary settlement", got, want)
			}
		}
		assertTargets(false)
		visitedTargets := map[server.Id]int{}
		var interruptedId server.Id
		virtualElapsed := time.Duration(0)
		bounded, cancelBudget := context.WithCancelCause(ctx)
		defer cancelBudget(context.Canceled)
		settle := func(callCtx context.Context, id server.Id, wait *legacySettlementGrantWait) (bool, bool, legacySettlementBusyGate, error) {
			// Virtual100ms per visit: an exact150-visit prefix owns15s.
			// No sleep or wall-clock deadline determines this assertion.
			if virtualElapsed >= 15*time.Second {
				interruptedId = id
				cancelBudget(errLegacySettlementPageBudget)
				return false, false, legacySettlementBusyNone, context.Canceled
			}
			virtualElapsed += 100 * time.Millisecond
			if id == targets[0] || id == targets[1] {
				visitedTargets[id]++
				return flushLegacySettlementWithGrantWait(callCtx, id, wait)
			}
			server.Tx(callCtx, func(tx server.PgTx) {
				if server.RaisePgResult(tx.Exec(callCtx, `DELETE FROM legacy_settlement_intent WHERE contract_id=$1`, id)).RowsAffected() != 1 {
					t.Fatal("synthetic predecessor was selected twice")
				}
			})
			return true, false, legacySettlementBusyNone, nil
		}
		before, err := json.Marshal(cursor)
		server.Raise(err)
		page, err := flushLegacySettlementsPage(ctx, bounded, 1, cursor, limit, settle)
		after, marshalErr := json.Marshal(cursor)
		server.Raise(marshalErr)
		wantVisits := min(limit, 150)
		if err != nil || page.Visited != wantVisits || page.Completed != wantVisits || page.Failed != 0 || !page.More || page.Cursor == nil || len(visitedTargets) != 0 || string(before) != string(after) {
			t.Fatalf("bounded dense prefix did not preserve untouched target authority: %+v targets=%d err=%v", page, len(visitedTargets), err)
		}
		wantHeads := (wantVisits + 2) / 4
		wantForward := wantVisits - wantHeads
		if page.HeadVisited != wantHeads || page.Cursor.HeadAfter == nil ||
			page.Cursor.ContractId != identity(uint32(10000+wantForward)) ||
			!page.Cursor.NextAttemptTime.Equal(oldest.Add(time.Minute+time.Duration(wantForward)*time.Millisecond)) ||
			page.Cursor.HeadAfter.ContractId != identity(uint32(wantHeads)) ||
			!page.Cursor.HeadAfter.NextAttemptTime.Equal(oldest.Add(time.Duration(wantHeads)*time.Millisecond)) ||
			!page.Cursor.PassEndTime.Equal(cursor.PassEndTime) {
			t.Fatal("dense page changed exact head/forward allocation or committed cursor", page)
		}
		if limit == 256 {
			if interruptedId != identity(10113) || context.Cause(bounded) != errLegacySettlementPageBudget {
				t.Fatal("virtual budget did not interrupt the exact151st forward intent")
			}
			server.Db(ctx, func(conn server.PgConn) {
				var retained bool
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1 AND failure_code='none' AND next_attempt_time=$2)`, interruptedId, oldest.Add(time.Minute+113*time.Millisecond)).Scan(&retained))
				if !retained {
					t.Fatal("virtual budget advanced or lost the interrupted intent")
				}
			})
		}
		assertTargets(false)
		// Intervention touches only synthetic precursor intents. Reports,
		// escrow, grants and target due tuples remain byte-for-byte inputs.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM legacy_settlement_intent WHERE contract_id=ANY($1)`, prefixIds))
		})
		bounded, cancelBudget = context.WithCancelCause(ctx)
		defer cancelBudget(context.Canceled)
		virtualElapsed = 0
		var retained *LegacySettlementCursor
		encoded, err := json.Marshal(page.Cursor)
		server.Raise(err)
		server.Raise(json.Unmarshal(encoded, &retained))
		healthy, err := flushLegacySettlementsPage(ctx, bounded, 1, retained, limit, settle)
		if err != nil || healthy.Completed != 2 || healthy.Failed != 0 || healthy.Cursor != nil || visitedTargets[targets[0]] != 1 || visitedTargets[targets[1]] != 1 {
			t.Fatalf("same untouched targets failed after precursor-only intervention: %+v err=%v", healthy, err)
		}
		assertTargets(true)
		projectLegacyProviderTotalsForTest(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var bytes, revenue int64
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count,provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$1`, f.destinationNetworkId).Scan(&bytes, &revenue))
			if bytes != 2 || revenue != 2 {
				t.Fatal("tagged target provider allocation did not conserve", bytes, revenue)
			}
		})
		proofs := map[server.Id][]byte{}
		readProof := func(id server.Id) []byte {
			var raw []byte
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&raw))
			})
			proof, err := decodeContractUsageSnapshot(raw)
			if err != nil || proof == nil || proof.ByteCount != 1 || len(proof.Providers) != 1 || proof.Providers[0].NetworkId != f.destinationNetworkId || proof.Providers[0].ClientId != f.destinationId || proof.Providers[0].ByteCount != 1 {
				t.Fatal("normal settlement did not retain exact immutable target usage", err)
			}
			return raw
		}
		for _, id := range targets {
			proofs[id] = readProof(id)
		}
		for _, id := range targets {
			complete, busy, _, err := flushLegacySettlement(ctx, id)
			if err != nil || complete || !busy {
				t.Fatal("tagged completed target replay reclaimed its financial owner")
			}
		}
		assertTargets(true)
		for _, id := range targets {
			if !bytes.Equal(proofs[id], readProof(id)) {
				t.Fatal("replay changed immutable target usage proof")
			}
		}
		t.Logf("target_density page_limit=%d precursors_each=%d dense_visits=%d dense_target_visits=0 virtual_budget_ms=15000 healthy_target_visits=2 target_settled=2; synthetic predecessor callbacks are not financial throughput", limit, predecessors, page.Visited)
	})
}

func legacyTargetDensityPlan(t testing.TB, ctx context.Context, lower, upper time.Time, target server.Id, expected int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var raw []byte
		server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON)
			SELECT next_attempt_time,contract_id FROM legacy_settlement_intent
			WHERE shard=1 AND next_attempt_time>$1 AND (next_attempt_time,contract_id)<($2,$3)
			AND next_attempt_time<=statement_timestamp() AT TIME ZONE 'UTC'
			ORDER BY next_attempt_time,contract_id LIMIT 8193`, lower, upper, target).Scan(&raw))
		var values []map[string]any
		server.Raise(json.Unmarshal(raw, &values))
		if len(values) != 1 || values[0]["Plan"].(map[string]any)["Actual Rows"] != float64(expected) {
			t.Fatal("bounded indexed rank fixture did not reproduce the actual8193 lower bound")
		}
		indexScans := 0
		var walk func(map[string]any)
		walk = func(node map[string]any) {
			switch node["Node Type"] {
			case "Seq Scan", "Sort", "Bitmap Heap Scan":
				t.Fatal("due rank used an unbounded or sorting access path", node["Node Type"])
			case "Index Scan", "Index Only Scan":
				condition, _ := node["Index Cond"].(string)
				if node["Index Name"] != "legacy_settlement_intent_due" || !strings.Contains(condition, "shard") || !strings.Contains(condition, "next_attempt_time") {
					t.Fatal("due rank lost its composite indexed bound", node)
				}
				indexScans++
			}
			if children, ok := node["Plans"].([]any); ok {
				for _, child := range children {
					walk(child.(map[string]any))
				}
			}
		}
		walk(values[0]["Plan"].(map[string]any))
		if indexScans != 1 {
			t.Fatal("due rank did not use exactly one bounded due-index scan", indexScans)
		}
		t.Logf("target_density_rank_plan=%s", raw)
	})
}
