package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Existing NULL metadata is discovered through its own capped index and never
// waits for an intent owner. The insert trigger also covers rolling old writers;
// the new enqueue path fills the key in its existing contract point read.
func TestLegacySettlementPayerRegistrationCompatibility(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, first := legacySettlementTestIntent(t, ctx)
		const count = 8193
		ids := make([]server.Id, count)
		prefix := server.NewId()
		for i := range ids {
			ids[i] = prefix
			binary.BigEndian.PutUint32(ids[i][11:15], uint32(i+1))
			ids[i][15] = 1
		}
		future := server.NowUtc().Add(365 * 24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
			 (contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count)
			 SELECT id,$2,$3,$4,$5,$2,0 FROM unnest($1::uuid[]) AS item(id)`, ids,
				f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
			// Deliberately omit the new field exactly as a rolling old writer.
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
			 SELECT id,1,'settled',$2 FROM unnest($1::uuid[]) AS item(id)`, ids, future))
			var registered int
			server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1) AND payer_network_id=$2`, ids, f.sourceNetworkId).Scan(&registered))
			if registered != count {
				t.Fatal("rolling writer did not get its exact payer", registered)
			}
			// Model rows that existed before migration791 without replaying
			// their financial transaction or changing retry/ordering values.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=NULL,source_client_id=NULL WHERE contract_id=ANY($1)`, ids))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE legacy_settlement_intent; ANALYZE transfer_contract`))
		})
		releaseHeld := holdContractCloseTestRow(t, ctx,
			`SELECT contract_id FROM legacy_settlement_intent WHERE contract_id=$1 FOR UPDATE`, ids[0])
		defer releaseHeld()
		checkLegacySettlementPayerRegistrationPlan(t, ctx)
		registered := 0
		for turn := 0; turn < 32; turn++ {
			got := registerLegacySettlementPayers(ctx, 1)
			if got != legacySettlementPayerRegistrationLimit {
				t.Fatal("uncapped or stalled registration", turn, got)
			}
			registered += got
		}
		if got := registerLegacySettlementPayers(ctx, 1); got != 0 || registered != count-1 {
			t.Fatal("held intent blocked finite registration", got, registered)
		}
		releaseHeld()
		if got := registerLegacySettlementPayers(ctx, 1); got != 1 {
			t.Fatal("released old intent was skipped", got)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT count(*)=$3 AND bool_and(payer_network_id=$2 AND next_attempt_time=$4 AND failure_code='none' AND outcome='settled')
			 FROM legacy_settlement_intent WHERE contract_id=ANY($1)`, ids, f.sourceNetworkId, count, future).Scan(&exact))
			if !exact {
				t.Fatal("registration changed due/failure/outcome or lost a payer")
			}
		})
		// Both indexed seeks must jump directly over the dense, future-only
		// payer. They cannot filter 8,193 future rows before reaching LIMIT1.
		checkLegacySettlementPayerPlan(t, ctx, legacySettlementPayerBeginSql, []any{1}, 1)
		checkLegacySettlementPayerPlan(t, ctx, legacySettlementPayerNextSql+` AND payer_network_id>$3 ORDER BY payer_network_id LIMIT 1`, []any{1, f.sourceNetworkId, f.sourceNetworkId}, 0)
		checkLegacySettlementPayerPlan(t, ctx, legacySettlementPayerIntentSql, []any{1, f.sourceNetworkId, server.NowUtc()}, -1)
		// A retry duplicate fills missing scheduling metadata, preserving the
		// accepted outcome and accounting delay rather than making it due.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=NULL,source_client_id=NULL,failure_code='accounting',next_attempt_time=$2 WHERE contract_id=$1`, first, future))
			server.Raise(queueLegacySettlementInTx(ctx, tx, first, ContractOutcomeSettled, false))
			var exact bool
			server.Raise(tx.QueryRow(ctx, `SELECT payer_network_id=$2 AND failure_code='accounting' AND next_attempt_time=$3 FROM legacy_settlement_intent WHERE contract_id=$1`, first, f.sourceNetworkId, future).Scan(&exact))
			if !exact {
				t.Fatal("enqueue metadata repair changed financial retry eligibility")
			}
		})
		requireLegacySettlementTestState(t, ctx, f, first, true, false, 1000, 100)
	})
}

func checkLegacySettlementPayerPlan(t testing.TB, ctx context.Context, query string, args []any, wantRows int, ownerIndex ...string) {
	t.Helper()
	indexName := "legacy_settlement_intent_payer_due"
	if len(ownerIndex) > 0 {
		indexName = ownerIndex[0]
	}
	server.Db(ctx, func(conn server.PgConn) {
		var raw []byte
		server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+query, args...).Scan(&raw))
		var plans []map[string]any
		server.Raise(json.Unmarshal(raw, &plans))
		root := plans[0]["Plan"].(map[string]any)
		if wantRows >= 0 && root["Actual Rows"] != float64(wantRows) {
			t.Fatal("unexpected bounded payer plan cardinality", string(raw))
		}
		indexed := false
		var walk func(map[string]any)
		walk = func(node map[string]any) {
			if node["Relation Name"] == "legacy_settlement_intent" {
				if node["Index Name"] != indexName || node["Node Type"] == "Seq Scan" {
					t.Fatal("payer selection lost its direct lane index", string(raw))
				}
				indexed = true
				if rows, ok := node["Rows Removed by Filter"].(float64); ok && rows > 1 {
					t.Fatal("payer selection scanned a dense prefix", string(raw))
				}
				if rows, ok := node["Actual Rows"].(float64); ok && rows > 1 {
					t.Fatal("payer selection escaped its row cap", string(raw))
				}
			}
			if children, ok := node["Plans"].([]any); ok {
				for _, child := range children {
					walk(child.(map[string]any))
				}
			}
		}
		walk(root)
		if !indexed {
			t.Fatal("payer seek has no indexed access", string(raw))
		}
		t.Logf("payer_index_bound rows=%v buffers_hit=%v buffers_read=%v", root["Actual Rows"], root["Shared Hit Blocks"], root["Shared Read Blocks"])
	})
}

// Retained payer metadata and actual escrow identify the payer consistently
// for rolling inserts and current queue writers, including legacy companions.
func TestLegacySettlementPayerCompanionAndNullOrigin(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		for _, c := range []struct {
			payer     *server.Id
			companion *server.Id
			want      server.Id
		}{
			{&f.destinationNetworkId, nil, f.destinationNetworkId},
			{nil, &id, f.sourceNetworkId},
			{nil, nil, f.sourceNetworkId},
			{&f.sourceNetworkId, &id, f.sourceNetworkId},
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=$2,companion_contract_id=$3 WHERE contract_id=$1`, id, c.payer, c.companion))
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM legacy_settlement_intent WHERE contract_id=$1`, id))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome) VALUES($1,$2,'settled')`, id, int(id[15])%16))
				var got server.Id
				server.Raise(tx.QueryRow(ctx, `SELECT payer_network_id FROM legacy_settlement_intent WHERE contract_id=$1`, id).Scan(&got))
				if got != c.want {
					t.Fatal("rolling writer chose source instead of accounting payer")
				}
				server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=NULL,source_client_id=NULL WHERE contract_id=$1`, id))
				server.Raise(queueLegacySettlementInTx(ctx, tx, id, ContractOutcomeSettled, false))
				server.Raise(tx.QueryRow(ctx, `SELECT payer_network_id FROM legacy_settlement_intent WHERE contract_id=$1`, id).Scan(&got))
				want := c.want
				if c.payer == nil {
					want = f.sourceNetworkId
				}
				if got != want {
					t.Fatal("new queue did not replace the legacy hint with the actual escrow payer")
				}
			})
		}
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
	})
}

// Round state survives an empty chronological remainder, JSON reconstruction,
// and new payer arrivals. Future-only lanes consume bounded probes and cannot
// strand the final due payer by restarting discovery at the lowest key.
func TestLegacySettlementPayerRoundRestartAndArrivals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, target := legacySettlementTestIntent(t, ctx)
		shard := int(target[15]) % 16
		highest := f.sourceNetworkId
		future := server.NowUtc().Add(365 * 24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			// Synthetic future-only contracts carry their declared payer in the
			// retained header. No fixture grant or financial authority changes.
			for i := 1; i <= 16; i++ {
				id := server.NewId()
				id[15] = target[15]
				payer := server.Id{}
				payer[15] = byte(i)
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count)
				 VALUES($1,$2,$3,$4,$5,$6,0)`, id, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, payer))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time,payer_network_id) VALUES($1,$2,'settled',$3,$4)`, id, shard, future, payer))
			}
		})
		oldCursor := &LegacySettlementCursor{NextAttemptTime: future, ContractId: server.NewId(), PassEndTime: future}
		before, _ := json.Marshal(oldCursor)
		first, err := FlushLegacySettlementShard(ctx, shard, oldCursor, nil, 64)
		after, _ := json.Marshal(oldCursor)
		if err != nil || first.Visited != 0 || first.PayerProbes != 16 || first.PayerCursor == nil || first.Cursor != nil || !first.More || !bytes.Equal(before, after) {
			t.Fatalf("empty lane probes lost restart state: %+v %v", first, err)
		}
		var resumed *LegacySettlementPayerCursor
		raw, _ := json.Marshal(first.PayerCursor)
		server.Raise(json.Unmarshal(raw, &resumed))
		server.Tx(ctx, func(tx server.PgTx) {
			id := server.NewId()
			id[15] = target[15]
			payer := highest
			payer[0]++
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count)
			 VALUES($1,$2,$3,$4,$5,$6,0)`, id, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, payer))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time,payer_network_id) VALUES($1,$2,'settled',$3,$4)`, id, shard, future, payer))
			// A new registration inside the remaining key range may join this
			// round. It consumes one empty probe rather than extending its due
			// cutoff or restarting at the old future-only prefix.
			inside := server.NewId()
			inside[15] = target[15]
			payer = server.Id{}
			payer[14] = 1
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count)
			 VALUES($1,$2,$3,$4,$5,$6,0)`, inside, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, payer))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time,payer_network_id) VALUES($1,$2,'settled',$3,$4)`, inside, shard, future, payer))
		})
		second, err := FlushLegacySettlementShard(ctx, shard, first.Cursor, resumed, 64)
		if err != nil || second.PayerVisited != 1 || second.PayerCompleted != 1 || second.PayerProbes != 2 || second.PayerCursor != nil {
			t.Fatalf("new arrivals extended the saved round: %+v %v", second, err)
		}
		requireLegacySettlementTestState(t, ctx, f, target, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, f, target, 11)
	})
}

// A native outcome trigger holds the second payer after the first financial
// commit. Ending the owning budget at that barrier retains exactly that prefix,
// including its payer cursor; replay settles the untouched second owner once.
func TestLegacySettlementPayerOwnedBudgetRetainsCommittedPrefix(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		firstOwner, first := legacySettlementTestIntent(t, ctx)
		secondOwner, second := legacySettlementTestIntent(t, ctx)
		if firstOwner.sourceNetworkId.Cmp(secondOwner.sourceNetworkId) > 0 {
			firstOwner, secondOwner = secondOwner, firstOwner
			first, second = second, first
		}
		shard := int(first[15]) % 16
		server.Tx(ctx, func(tx server.PgTx) {
			// Move the second identity before queueing it into the same shard.
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM legacy_settlement_intent WHERE contract_id=$1`, second))
			newId := second
			newId[15] = first[15]
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET contract_id=$2 WHERE contract_id=$1`, second, newId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET contract_id=$2 WHERE contract_id=$1`, second, newId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET contract_id=$2 WHERE contract_id=$1`, second, newId))
			second = newId
			server.Raise(queueLegacySettlementInTx(ctx, tx, second, ContractOutcomeSettled, false))
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION synthetic_payer_budget_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN IF NEW.contract_id=TG_ARGV[0]::uuid THEN PERFORM set_config('lock_timeout','0',true); PERFORM pg_advisory_xact_lock(731071); END IF; RETURN NEW; END $$;
			 CREATE TRIGGER synthetic_payer_budget_barrier BEFORE UPDATE OF outcome ON transfer_contract FOR EACH ROW
			 WHEN (OLD.outcome IS NULL AND NEW.outcome IS NOT NULL) EXECUTE FUNCTION synthetic_payer_budget_barrier('%s');`, second)))
		})
		refreshNetEscrow(ctx, []server.Id{firstOwner.balanceId, secondOwner.balanceId})
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT pg_advisory_xact_lock(731071)`))
		blocker := contractLifecycleTestBackendPid(t, ctx, held)
		bounded, expire := context.WithCancelCause(ctx)
		defer expire(context.Canceled)
		type outcome struct {
			page LegacySettlementShardResult
			err  error
		}
		done := make(chan outcome, 1)
		go func() {
			page, err := flushLegacySettlementShardPage(ctx, bounded, shard, nil, nil, 64)
			done <- outcome{page: page, err: err}
		}()
		requireContractLifecycleBlockedBy(t, ctx, held, blocker)
		expire(errLegacySettlementPageBudget)
		var got outcome
		select {
		case got = <-done:
		case <-ctx.Done():
			t.Fatal("payer owner failed to join its bounded operation", ctx.Err())
		}
		server.Raise(held.Rollback(ctx))
		conn.Release()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_payer_budget_barrier ON transfer_contract; DROP FUNCTION synthetic_payer_budget_barrier()`))
		})
		if got.err != nil || got.page.Visited != 1 || got.page.Completed != 1 || got.page.Failed != 0 || got.page.PayerVisited != 1 || got.page.PayerCursor == nil || got.page.PayerCursor.After == nil || !got.page.More {
			t.Fatalf("payer deadline lost exact committed prefix: %+v %v", got.page, got.err)
		}
		requireLegacySettlementTestState(t, ctx, firstOwner, first, false, true, 989, 0)
		requireLegacySettlementTestState(t, ctx, secondOwner, second, true, false, 1000, 100)
		resumed, err := FlushLegacySettlementShard(ctx, shard, got.page.Cursor, got.page.PayerCursor, 64)
		if err != nil || resumed.Completed != 1 || resumed.PayerCompleted != 1 || resumed.Failed != 0 {
			t.Fatalf("payer deadline replay failed: %+v %v", resumed, err)
		}
		requireLegacySettlementTestState(t, ctx, secondOwner, second, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, firstOwner, first, 11)
		requireLegacyProviderDurability(t, ctx, secondOwner, second, 11)
	})
}

// Optional metadata repair rolls back on its own statement budget. Holding its
// update at a native barrier cannot deny an already registered funded payer.
func TestLegacySettlementPayerRegistrationTimeoutFallsBack(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f, target := legacySettlementTestIntent(t, ctx)
		future := server.NewId()
		future[15] = target[15]
		shard := int(target[15]) % 16
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count)
			 VALUES($1,$2,$3,$4,$5,$2,0)`, future, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time) VALUES($1,$2,'settled',$3)`, future, shard, server.NowUtc().Add(time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=NULL,source_client_id=NULL WHERE contract_id=$1`, future))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_payer_registration_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN PERFORM pg_advisory_xact_lock(731072); RETURN NEW; END $$;
			 CREATE TRIGGER synthetic_payer_registration_barrier BEFORE UPDATE OF payer_network_id ON legacy_settlement_intent
			 FOR EACH ROW WHEN (OLD.payer_network_id IS NULL) EXECUTE FUNCTION synthetic_payer_registration_barrier()`))
		})
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT pg_advisory_xact_lock(731072)`))
		blocker := contractLifecycleTestBackendPid(t, ctx, held)
		type outcome struct {
			page LegacySettlementShardResult
			err  error
		}
		done := make(chan outcome, 1)
		go func() {
			page, err := FlushLegacySettlementShard(ctx, shard, nil, nil, 64)
			done <- outcome{page: page, err: err}
		}()
		requireContractLifecycleBlockedBy(t, ctx, held, blocker)
		var got outcome
		select {
		case got = <-done:
		case <-ctx.Done():
			t.Fatal("payer owner failed to join its bounded operation", ctx.Err())
		}
		server.Raise(held.Rollback(ctx))
		conn.Release()
		if got.err != nil || !got.page.PayerRegistrationFailed || got.page.PayerRegistered != 0 || got.page.PayerCompleted != 1 || got.page.Completed != 1 || got.page.Failed != 0 {
			t.Fatalf("optional registration timeout blocked settlement: %+v %v", got.page, got.err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var missing bool
			server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id IS NULL FROM legacy_settlement_intent WHERE contract_id=$1`, future).Scan(&missing))
			if !missing {
				t.Fatal("timed-out registration did not roll back")
			}
		})
		requireLegacySettlementTestState(t, ctx, f, target, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, f, target, 11)
	})
}

// A failed retry write is not a committed payer turn. Retain both input
// positions and expose the error, while the rolled-back accounting keeps debt.
func TestLegacySettlementPayerRetryWriteFailureKeepsCursor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		shard := int(id[15]) % 16
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=101 WHERE contract_id=$1`, id))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_payer_retry_refusal() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN RAISE EXCEPTION 'synthetic retry-state refusal' USING ERRCODE='23514'; END $$;
			 CREATE TRIGGER synthetic_payer_retry_refusal BEFORE UPDATE OF failure_code ON legacy_settlement_intent FOR EACH ROW
			 EXECUTE FUNCTION synthetic_payer_retry_refusal()`))
		})
		ordered := &LegacySettlementCursor{NextAttemptTime: server.NowUtc().Add(-time.Hour), ContractId: server.NewId(), PassEndTime: server.NowUtc()}
		payer := beginLegacySettlementPayerRound(ctx, shard)
		before, _ := json.Marshal(payer)
		page, err := FlushLegacySettlementShard(ctx, shard, ordered, payer, 64)
		after, _ := json.Marshal(payer)
		if err == nil || page.Failed != 1 || page.PayerFailed != 1 || page.Completed != 0 || page.PayerCursor == nil || page.PayerCursor.After != nil || page.Cursor != ordered || !bytes.Equal(before, after) {
			t.Fatalf("failed retry write advanced a cursor: %+v %v", page, err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT failure_code='none' AND next_attempt_time<=statement_timestamp() AT TIME ZONE 'UTC' FROM legacy_settlement_intent WHERE contract_id=$1`, id).Scan(&exact))
			if !exact {
				t.Fatal("failed retry write changed original intent")
			}
		})
	})
}

// Plan the exact registration selector at density with an actually locked row.
// LIMIT precedes the contract lookup; at most256 PK probes can be performed.
func checkLegacySettlementPayerRegistrationPlan(t testing.TB, ctx context.Context) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		var raw []byte
		var end server.Id
		for index := range end {
			end[index] = 0xff
		}
		server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+legacyCloseOwnerRegistrationPageSql, 1, end).Scan(&raw))
		var plans []map[string]any
		server.Raise(json.Unmarshal(raw, &plans))
		root := plans[0]["Plan"].(map[string]any)
		if root["Actual Rows"] != float64(256) {
			t.Fatal("registration exceeded its cap", string(raw))
		}
		intentIndex, contractIndex := false, false
		var walk func(map[string]any)
		walk = func(node map[string]any) {
			switch node["Relation Name"] {
			case "legacy_settlement_intent":
				intentIndex = node["Index Name"] == "legacy_settlement_intent_owner_missing"
				if !intentIndex || node["Actual Rows"].(float64) > 257 {
					t.Fatal("registration scanned the unbounded queue", string(raw))
				}
			case "transfer_contract":
				contractIndex = node["Index Name"] == "transfer_contract_pkey"
				if !contractIndex || node["Actual Rows"].(float64) > 1 || node["Actual Loops"].(float64) > 256 {
					t.Fatal("registration lost contract PK bounds", string(raw))
				}
			}
			if children, ok := node["Plans"].([]any); ok {
				for _, child := range children {
					walk(child.(map[string]any))
				}
			}
		}
		walk(root)
		if !intentIndex || !contractIndex {
			t.Fatal("registration plan omitted a required bounded index", string(raw))
		}
		t.Logf("payer_registration_plan rows=%v buffers_hit=%v buffers_read=%v", root["Actual Rows"], root["Shared Hit Blocks"], root["Shared Read Blocks"])
	}, server.TxReadCommitted, server.OptNoRetry())
}
