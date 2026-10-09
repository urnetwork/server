// Discovery must repair a populated stale owner marker before normal billing.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A current old-writer INSERT omits both routing hints. The real trigger reads
// retained payer/escrow authority and records source identity for paid work too.
func TestLegacyCloseOwnerInsertKeepsPaidSourceMarker(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		for _, clearCache := range []bool{false, true} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM legacy_settlement_intent WHERE contract_id=$1`, id))
				if clearCache {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=NULL WHERE contract_id=$1`, id))
				}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome)
					VALUES($1,$2,'settled')`, id, int(id[15])%LegacySettlementShardCount))
				var exact bool
				server.Raise(tx.QueryRow(ctx, `SELECT payer_network_id IS NOT DISTINCT FROM $2::uuid AND source_client_id IS NOT DISTINCT FROM $3::uuid
					FROM legacy_settlement_intent WHERE contract_id=$1`, id, f.sourceNetworkId, f.sourceId).Scan(&exact))
				if !exact {
					t.Fatal("current INSERT classified actual paid escrow as source-only work", clearCache)
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
		}
	})
}

// This is an explicit retained-state seam, not a claim that today's INSERT
// creates the mismatch. The trigger permits historical NULL-marker updates;
// a stale classification can therefore be represented without disabling it.
func retainLegacyDispatchStaleHint(t testing.TB, ctx context.Context, id server.Id, payerHint *server.Id, sourceHint server.Id) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent
			SET payer_network_id=NULL,source_client_id=NULL WHERE contract_id=$1`, id))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent
			SET payer_network_id=$2,source_client_id=$3 WHERE contract_id=$1`, id, payerHint, sourceHint))
		var exact bool
		server.Raise(tx.QueryRow(ctx, `SELECT payer_network_id IS NOT DISTINCT FROM $2::uuid
			AND source_client_id IS NOT DISTINCT FROM $3::uuid FROM legacy_settlement_intent WHERE contract_id=$1`,
			id, payerHint, sourceHint).Scan(&exact))
		if !exact {
			t.Fatal("retained-state fixture did not preserve its explicit stale owner hints")
		}
	}, server.TxReadCommitted, server.OptNoRetry())
}

// Routing hints alone may change. One statement compares the complete accepted
// intent, contract/proof, reports, grants, debit, payout and financial balance.
func legacyDispatchAuthorityState(ctx context.Context, id, balanceId server.Id) (state string) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT jsonb_build_array(to_jsonb(c),
			(SELECT to_jsonb(i)-'payer_network_id'-'source_client_id' FROM legacy_settlement_intent i WHERE contract_id=$1),
			(SELECT jsonb_agg(to_jsonb(r) ORDER BY party) FROM contract_close r WHERE contract_id=$1),
			(SELECT jsonb_agg(to_jsonb(e) ORDER BY balance_id) FROM transfer_escrow e WHERE contract_id=$1),
			(SELECT jsonb_agg(to_jsonb(s) ORDER BY balance_id) FROM transfer_escrow_sweep s WHERE contract_id=$1),
			(SELECT jsonb_agg(to_jsonb(j) ORDER BY balance_id) FROM transfer_debit_journal j WHERE contract_id=$1),
			(SELECT to_jsonb(b) FROM transfer_balance b WHERE balance_id=$2))::text
			FROM transfer_contract c WHERE contract_id=$1`, id, balanceId).Scan(&state))
	})
	return
}

// A startup deadline already wakes the correct payer, but its indexed page is
// empty while the accepted intent still names a source or another payer. The
// recurring dispatcher must repair just that hint, then normal billing closes.
func TestLegacyCloseDispatcherRepairsPopulatedStalePaidHint(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, kind := range []string{"source", "other-payer", "other-source"} {
			f, id := legacySettlementTestIntent(t, ctx)
			deadline := server.NowUtc().Truncate(time.Microsecond).Add(-time.Minute)
			server.Tx(ctx, func(tx server.PgTx) {
				var locked server.Id
				server.Raise(tx.QueryRow(ctx, `SELECT contract_id FROM legacy_settlement_intent
					WHERE contract_id=$1 FOR UPDATE`, id).Scan(&locked))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, id, deadline))
				proof, err := prepareContractExpiryInTx(ctx, tx, id, server.NowUtc())
				server.Raise(err)
				if proof == nil {
					t.Fatal("retained intent fixture did not acquire its ordinary expiry proof")
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			var payerHint *server.Id
			sourceHint := f.sourceId
			if kind == "other-payer" {
				payerHint = &f.destinationNetworkId
			} else if kind == "other-source" {
				sourceHint = server.NewId()
			}
			retainLegacyDispatchStaleHint(t, ctx, id, payerHint, sourceHint)
			before := legacyDispatchAuthorityState(ctx, id, f.balanceId)
			owner, err := CloseContractAtDeadline(ctx, id, deadline)
			if err != nil || owner == nil || *owner != (ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: f.sourceNetworkId}) {
				t.Fatal("startup deadline did not retain the actual financial payer", kind, owner, err)
			}
			client := session.NewLocalClientSession(ctx, "", nil)
			defer client.Cancel()
			settings := task.DefaultTaskWorkerSettings()
			settings.ClaimRegisteredTargetsOnly = true
			worker := task.NewTaskWorker(ctx, settings)
			defer worker.Close()
			worker.AddTargets(NewLegacyPayerSettlementTaskTarget())
			runPayer := func() {
				withLegacyPayerQueueTestTx(ctx, []server.Id{owner.Id}, func(tx server.PgTx) {
					// Use the ordinary scheduler with an already-due fixture wake;
					// PostgreSQL computes claim eligibility from run_at itself.
					ScheduleLegacyCloseSettlementsInTx(client, tx, *owner, nil, time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC))
				})
				var taskId server.Id
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`,
						task.RunOnce("flush_legacy_payer_settlements", owner.Id).String()).Scan(&taskId))
				})
				finished, retried, posts, err := worker.EvalTasks(1)
				if err != nil || len(finished) != 1 || finished[0] != taskId || len(retried)+len(posts) != 0 {
					t.Fatal("actual payer task did not finish its ordinary bounded turn", kind, finished, retried, posts, err)
				}
			}
			runPayer()
			if legacyDispatchAuthorityState(ctx, id, f.balanceId) != before {
				t.Fatal("empty correct-payer page changed another routing scope", kind)
			}
			shard := int(id[15]) % LegacySettlementShardCount
			first, err := DispatchLegacySettlementCloseOwners(ctx, shard, nil, nil, nil)
			if err != nil || first.RegistrationFailed || len(first.PayerNetworkIds)+len(first.SourceClientIds) != 0 {
				t.Fatal("stale discovery published work under the wrong owner", kind, first, err)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var repaired bool
				server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id IS NOT DISTINCT FROM $2::uuid AND source_client_id IS NOT DISTINCT FROM $3::uuid
					FROM legacy_settlement_intent WHERE contract_id=$1`, id, f.sourceNetworkId, f.sourceId).Scan(&repaired))
				if !repaired {
					t.Fatal("populated stale owner marker never entered exact registration", kind)
				}
			})
			if legacyDispatchAuthorityState(ctx, id, f.balanceId) != before {
				t.Fatal("exact registration changed accepted intent, proof, reports or money", kind)
			}
			requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
			second, err := DispatchLegacySettlementCloseOwners(ctx, shard, first.Cursor, first.PayerCursor, first.SourceCursor, first.RegistrationCursor)
			if err != nil || len(second.PayerNetworkIds) != 1 || second.PayerNetworkIds[0] != owner.Id || len(second.SourceClientIds) != 0 {
				t.Fatal("next discovery round did not publish the repaired actual payer", kind, second, err)
			}
			runPayer()
			requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
			requireLegacyProviderDurability(t, ctx, f, id, 11)
		}
	})
}

// Exact registration must not wait while holding someone else's intent or
// contract. A later round repairs the same row after that owner commits.
func TestLegacyCloseDispatcherStaleHintSkipsBusyIntentAndContract(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		for _, table := range []string{"legacy_settlement_intent", "transfer_contract"} {
			f, id := legacySettlementTestIntent(t, ctx)
			retainLegacyDispatchStaleHint(t, ctx, id, nil, f.sourceId)
			before := legacyDispatchAuthorityState(ctx, id, f.balanceId)
			ready, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				var result error
				server.HandleError(func() {
					server.Tx(ctx, func(tx server.PgTx) {
						var locked server.Id
						server.Raise(tx.QueryRow(ctx, `SELECT contract_id FROM `+table+` WHERE contract_id=$1 FOR UPDATE`, id).Scan(&locked))
						close(ready)
						select {
						case <-release:
						case <-ctx.Done():
							server.Raise(ctx.Err())
						}
					}, server.TxReadCommitted, server.OptNoRetry())
				}, func(err error) { result = err })
				done <- result
			}()
			func() {
				defer func() {
					close(release)
					select {
					case err := <-done:
						if err != nil {
							t.Error("fixture lock owner failed", table, err)
						}
					case <-time.After(5 * time.Second):
						t.Error("fixture lock owner did not join", table)
					}
				}()
				select {
				case <-ready:
				case <-ctx.Done():
					t.Fatal("fixture lock owner did not become ready", table, ctx.Err())
				}
				result, err := DispatchLegacySettlementCloseOwners(ctx, int(id[15])%LegacySettlementShardCount, nil, nil, nil)
				if err != nil || result.RegistrationFailed || len(result.PayerNetworkIds)+len(result.SourceClientIds) != 0 || legacyDispatchAuthorityState(ctx, id, f.balanceId) != before {
					t.Fatal("busy exact registration changed custody or published the wrong owner", table, result, err)
				}
				server.Db(ctx, func(conn server.PgConn) {
					var unchanged bool
					server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id IS NULL AND source_client_id=$2
						FROM legacy_settlement_intent WHERE contract_id=$1`, id, f.sourceId).Scan(&unchanged))
					if !unchanged {
						t.Fatal("busy exact registration rewrote an unowned hint", table)
					}
				})
			}()
			_, err := DispatchLegacySettlementCloseOwners(ctx, int(id[15])%LegacySettlementShardCount, nil, nil, nil)
			server.Raise(err)
			server.Db(ctx, func(conn server.PgConn) {
				var repaired bool
				server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id IS NOT DISTINCT FROM $2::uuid AND source_client_id IS NOT DISTINCT FROM $3::uuid
					FROM legacy_settlement_intent WHERE contract_id=$1`, id, f.sourceNetworkId, f.sourceId).Scan(&repaired))
				if !repaired {
					t.Fatal("released stale owner was never revisited", table)
				}
			})
			page, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, 8)
			if err != nil || page.Completed != 1 || page.Failed != 0 {
				t.Fatal("repaired busy intent did not reach its actual owner", table, page, err)
			}
		}
	})
}

// A failure after UPDATE must not acknowledge a repaired route or alter the
// accepted close. The same exact head is repaired when its commit can succeed.
func TestLegacyCloseDispatcherStaleHintCommitRefusalKeepsCustody(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		retainLegacyDispatchStaleHint(t, ctx, id, nil, f.sourceId)
		before := legacyDispatchAuthorityState(ctx, id, f.balanceId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE SEQUENCE stale_owner_repair_commit;
				CREATE FUNCTION refuse_stale_owner_repair_commit() RETURNS trigger LANGUAGE plpgsql AS $body$
				BEGIN
					PERFORM nextval('stale_owner_repair_commit');
					RAISE EXCEPTION 'synthetic stale owner repair commit refusal';
				END $body$;
				CREATE CONSTRAINT TRIGGER refuse_stale_owner_repair_commit AFTER UPDATE ON legacy_settlement_intent
				DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION refuse_stale_owner_repair_commit()`))
		})
		shard := int(id[15]) % LegacySettlementShardCount
		result, err := DispatchLegacySettlementCloseOwners(ctx, shard, nil, nil, nil)
		if err == nil || len(result.PayerNetworkIds)+len(result.SourceClientIds) != 0 {
			t.Fatal("failed exact registration commit acknowledged the stale financial owner", result, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var refused, unchanged bool
			server.Raise(conn.QueryRow(ctx, `SELECT is_called AND last_value=1 FROM stale_owner_repair_commit`).Scan(&refused))
			server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id IS NULL AND source_client_id=$2
				FROM legacy_settlement_intent WHERE contract_id=$1`, id, f.sourceId).Scan(&unchanged))
			if !refused || !unchanged {
				t.Fatal("failed exact registration did not retain its original owner hints", refused, unchanged)
			}
		})
		if legacyDispatchAuthorityState(ctx, id, f.balanceId) != before {
			t.Fatal("failed exact registration changed accepted proof or financial custody")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER refuse_stale_owner_repair_commit ON legacy_settlement_intent;
				DROP FUNCTION refuse_stale_owner_repair_commit(); DROP SEQUENCE stale_owner_repair_commit`))
		})
		_, err = DispatchLegacySettlementCloseOwners(ctx, shard, nil, nil, nil)
		server.Raise(err)
		server.Db(ctx, func(conn server.PgConn) {
			var repaired bool
			server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id IS NOT DISTINCT FROM $2::uuid
				AND source_client_id IS NOT DISTINCT FROM $3::uuid FROM legacy_settlement_intent WHERE contract_id=$1`,
				id, f.sourceNetworkId, f.sourceId).Scan(&repaired))
			if !repaired {
				t.Fatal("commit-refused stale owner did not repair on its next turn")
			}
		})
		if legacyDispatchAuthorityState(ctx, id, f.balanceId) != before {
			t.Fatal("successful registration retry changed accepted proof or financial custody")
		}
	})
}
