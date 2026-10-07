package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The ordinary public page sees 8,193 indexed predecessors before an
// independently funded target. Both cases use real financial owners. Holding
// only the prefix's grant creates the busy case through a native row-lock
// barrier; the healthy prefix needs no timing or synthetic settlement stub.
// Moving only the target's due tuple immediately after the persisted forward
// cursor is the paired intervention. Reports, escrow, grants, usage inputs and
// all financial guards remain identical. This diagnoses bounded FIFO delay;
// it does not assert infinite starvation or measure production throughput.
func TestLegacySettlementDenseVisitationRealEntryHealthyPrefix(t *testing.T) {
	testLegacySettlementDenseVisitationRealEntry(t, false, false)
}

func TestLegacySettlementDenseVisitationRealEntryHeldPrefix(t *testing.T) {
	testLegacySettlementDenseVisitationRealEntry(t, true, false)
}

// The shard owner gives an independent payer service without changing its due
// tuple or financial inputs. The retained ordered-only controls above still
// reproduce the FIFO density mechanism.
func TestLegacySettlementPayerDenseHealthyService(t *testing.T) {
	testLegacySettlementDenseVisitationRealEntry(t, false, true)
}

func TestLegacySettlementPayerDenseHeldService(t *testing.T) {
	testLegacySettlementDenseVisitationRealEntry(t, true, true)
}

func testLegacySettlementDenseVisitationRealEntry(t *testing.T, holdPrefix, requireSparseService bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		prefixOwner := newNetEscrowOrderingTestFixture(t, ctx)
		targetOwner := newNetEscrowOrderingTestFixture(t, ctx)
		const predecessors = 8193
		const pageLimit = 256
		const boundedPages = 2
		prefix := server.NewId()
		identity := func(sequence uint32) server.Id {
			id := prefix
			binary.BigEndian.PutUint32(id[11:15], sequence)
			id[15] = 1
			return id
		}
		oldest := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
		prefixIds := make([]server.Id, predecessors)
		prefixTimes := make([]time.Time, predecessors)
		for index := range predecessors {
			prefixIds[index] = identity(uint32(index + 1))
			prefixTimes[index] = oldest.Add(time.Duration(index+1) * time.Millisecond)
		}
		target := identity(100000)
		targetDue := oldest.Add(time.Duration(predecessors+1) * time.Millisecond)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET
				start_balance_byte_count=20000,balance_byte_count=20000,net_revenue_nano_cents=40000
				WHERE balance_id=$1`, prefixOwner.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, targetOwner.balanceId))
			for _, cohort := range []struct {
				owner netEscrowOrderingTestFixture
				ids   []server.Id
				due   []time.Time
			}{{prefixOwner, prefixIds, prefixTimes}, {targetOwner, []server.Id{target}, []time.Time{targetDue}}} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
					SELECT id,$2,$3,$4,$5,$2,2,true FROM unnest($1::uuid[]) AS row(id)`,
					cohort.ids, cohort.owner.sourceNetworkId, cohort.owner.sourceId,
					cohort.owner.destinationNetworkId, cohort.owner.destinationId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
					SELECT id,$2,2 FROM unnest($1::uuid[]) AS row(id)`, cohort.ids, cohort.owner.balanceId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
					SELECT id,party,1,statement_timestamp() AT TIME ZONE 'UTC',false FROM unnest($1::uuid[]) AS row(id)
					CROSS JOIN (VALUES ('source'),('destination')) AS parties(party)`, cohort.ids))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,clear_dispute,next_attempt_time)
					SELECT id,1,'settled',false,due FROM unnest($1::uuid[],$2::timestamp[]) AS row(id,due)`, cohort.ids, cohort.due))
			}
		})
		refreshNetEscrow(ctx, []server.Id{prefixOwner.balanceId, targetOwner.balanceId})
		legacyTargetDensityPlan(t, ctx, oldest, targetDue, target, predecessors)

		// Returning from FOR UPDATE proves the prefix's real grant owner is
		// held before the first public page. No sleep orders this barrier.
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		if holdPrefix {
			server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, prefixOwner.balanceId))
		}
		capture := "00000000000000000000000000000001"
		plan := legacyTargetTracePlan{Capture: capture, TargetDigest: legacyTargetTraceDigest(capture, target), Shard: 1,
			Starts: time.Now(), Expires: time.Now().Add(time.Minute), MaxPages: 4}
		var payerCursor *LegacySettlementPayerCursor
		page := func(cursor *LegacySettlementCursor) LegacySettlementFlushResult {
			t.Helper()
			trace := newLegacyTargetTrace(plan, "00000000000000000000000000000002", "automatic_page", cursor, nil)
			callCtx := context.WithValue(ctx, legacyTargetTraceKey{}, trace)
			var result LegacySettlementFlushResult
			var err error
			if requireSparseService {
				var shardResult LegacySettlementShardResult
				shardResult, err = FlushLegacySettlementShard(callCtx, 1, cursor, payerCursor, pageLimit)
				encoded, marshalErr := json.Marshal(shardResult.PayerCursor)
				server.Raise(marshalErr)
				payerCursor = nil
				server.Raise(json.Unmarshal(encoded, &payerCursor))
				result = shardResult.LegacySettlementFlushResult
			} else {
				result, err = FlushLegacySettlements(callCtx, 1, cursor, pageLimit)
			}
			if err != nil || result.Failed != 0 || result.Visited <= 0 || result.Visited > pageLimit ||
				result.Cursor == nil || !result.More || result.Trace == nil {
				t.Fatalf("bounded public page failed: %+v err=%v", result, err)
			}
			if result.Timings == nil || result.Timings.Selection.Count < result.Visited || result.Timings.Financial.Count < result.Visited {
				t.Fatal("real public page omitted selection or financial phase observations")
			}
			phaseJSON, err := json.Marshal(result.Timings)
			server.Raise(err)
			t.Logf("dense_public_page visited=%d completed=%d busy=%d target_selected=%d phase_timings=%s; monotonic phase elapsed includes real pool acquisition/SQL and is not production throughput", result.Visited, result.Completed, result.BusyOrGone, result.Trace.Selected, phaseJSON)
			return result
		}
		assertTarget := func(settled bool) []byte {
			t.Helper()
			want := 0
			if settled {
				want = 1
			}
			var proof []byte
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM transfer_contract WHERE contract_id=$1 AND outcome='settled')=$3
					AND (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=$1 AND failure_code='none')=1-$3
					AND (SELECT count(*) FROM transfer_escrow WHERE contract_id=$1 AND settled)=$3
					AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=1000-$3
					AND (SELECT coalesce(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=$1)=$3
					AND (SELECT coalesce(sum(payout_net_revenue_nano_cents),0) FROM transfer_escrow_sweep WHERE contract_id=$1)=$3`,
					target, targetOwner.balanceId, want).Scan(&exact))
				if !exact {
					t.Fatal("target financial state changed outside ordinary ownership", settled)
				}
				server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, target).Scan(&proof))
			})
			if got := Testing_NetEscrowByteCount(ctx, targetOwner.balanceId); got != int64(2*(1-want)) {
				t.Fatal("target mirror reservation did not conserve", got, want)
			}
			if settled {
				decoded, err := decodeContractUsageSnapshot(proof)
				if err != nil || decoded == nil || decoded.ByteCount != 1 || len(decoded.Providers) != 1 ||
					decoded.Providers[0].NetworkId != targetOwner.destinationNetworkId ||
					decoded.Providers[0].ClientId != targetOwner.destinationId || decoded.Providers[0].ByteCount != 1 {
					t.Fatal("normal target owner did not preserve exact usage proof", err)
				}
			}
			return proof
		}
		assertTarget(false)
		var cursor *LegacySettlementCursor
		completed, busy, visited, selected := 0, 0, 0, 0
		for range boundedPages {
			before, err := json.Marshal(cursor)
			server.Raise(err)
			result := page(cursor)
			after, err := json.Marshal(cursor)
			server.Raise(err)
			if !bytes.Equal(before, after) || !requireSparseService && result.Trace.Selected != 0 {
				t.Fatal("dense page mutated its input or reached the distant target")
			}
			if holdPrefix && result.Completed != result.Trace.Selected || !holdPrefix && result.BusyOrGone != 0 {
				t.Fatal("held/healthy prefix intervention did not isolate ordinary ownership", result)
			}
			completed += result.Completed
			busy += result.BusyOrGone
			visited += result.Visited
			selected += result.Trace.Selected
			encoded, err := json.Marshal(result.Cursor)
			server.Raise(err)
			cursor = nil
			server.Raise(json.Unmarshal(encoded, &cursor))
			assertTarget(requireSparseService && selected > 0)
		}
		if requireSparseService {
			if selected != 1 {
				t.Fatalf("dense_sparse_account_service_bound: independent funded account had %d natural target entries after %d real public pages and %d visits behind %d healthy predecessors; require one", selected, boundedPages, visited, predecessors)
			}
			assertTarget(true)
		}
		if visited > boundedPages*pageLimit || cursor.NextAttemptTime.Compare(targetDue) >= 0 {
			t.Fatal("bounded traversal escaped its indexed predecessor interval")
		}
		// Only the queue-ordering variable changes. The same target must now
		// enter the first forward financial owner even while the prefix grant
		// remains held in the busy case. This is a fixture causal intervention,
		// not a production proposal to rewrite intent due times.
		if !requireSparseService {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`,
					target, cursor.NextAttemptTime.Add(time.Microsecond)))
			})
			healthy := page(cursor)
			if healthy.Trace.Selected != 1 || healthy.Completed < 1 {
				t.Fatal("same financial target failed after only its ordering position changed", healthy)
			}
		}
		proof := assertTarget(true)
		complete, busyAgain, _, err := flushLegacySettlement(ctx, target)
		if err != nil || complete || !busyAgain || !bytes.Equal(proof, assertTarget(true)) {
			t.Fatal("completed target replay repeated accounting or changed immutable proof", err)
		}
		server.Raise(held.Rollback(ctx))
		projectLegacyProviderTotalsForTest(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var provided, revenue int64
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count,provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$1`,
				targetOwner.destinationNetworkId).Scan(&provided, &revenue))
			if provided != 1 || revenue != 1 {
				t.Fatal("target provider allocation did not conserve", provided, revenue)
			}
			var prefixExact bool
			server.Raise(conn.QueryRow(ctx, `WITH finished AS (
				SELECT count(*) AS n FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'
			) SELECT
				(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1))=$3-f.n
				AND (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled)=f.n
				AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=20000-f.n
				AND (SELECT coalesce(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=f.n
				AND (SELECT coalesce(sum(payout_net_revenue_nano_cents),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=f.n
				AND coalesce((SELECT provided_byte_count FROM account_balance WHERE network_id=$4),0)=f.n
				AND coalesce((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$4),0)=f.n
				FROM finished f`, prefixIds, prefixOwner.balanceId, predecessors, prefixOwner.destinationNetworkId).Scan(&prefixExact))
			if !prefixExact {
				t.Fatal("dense prefix outcomes, intents, grant debit or provider allocation did not conserve")
			}
		})
		t.Logf("dense real public entry: prefix=%d bounded_pages=%d cap=%d hold_prefix=%t visited=%d completed=%d busy=%d payer_service=%t natural_target_selected=%d; counts are not a throughput forecast", predecessors, boundedPages, pageLimit, holdPrefix, visited, completed, busy, requireSparseService, selected)
	})
}
