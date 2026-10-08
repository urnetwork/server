// Durable task handoff tests exercise recovery and EOF through real owners.
package model

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Manual task handoffs use the same complete queue authority as the actual
// worker finalizer. The helper never acquires keys inside a business callback.
func withLegacyPayerQueueTestTx(ctx context.Context, payerIds []server.Id, callback func(server.PgTx)) {
	keys, err := LegacyPayerSettlementQueueOwnershipKeys(payerIds)
	server.Raise(err)
	server.OwnedTx(ctx, keys, callback, server.TxReadCommitted, server.OptNoRetry())
}

// Optional registration-index repair cannot stop already indexed payer work.
// The bounded chronological fallback also recovers a pre-migration NULL key.
func TestLegacyPayerDispatchRecoversWithoutMissingIndexOrWake(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP INDEX legacy_settlement_intent_payer_missing`))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=NULL WHERE contract_id=$1`, id))
		})
		// No close callback or wake is delivered. The durable intent is enough.
		result, err := DispatchLegacySettlementPayers(ctx, int(id[15])%16, nil, nil)
		if err != nil || result.RegistrationFailed || result.Registered != 1 || len(result.PayerNetworkIds) != 0 || !result.More {
			t.Fatal("durable discovery lost an intent while optional index was absent", result, err)
		}
		result, err = DispatchLegacySettlementPayers(ctx, int(id[15])%16, result.Cursor, result.PayerCursor)
		if err != nil || len(result.PayerNetworkIds) != 1 || result.PayerNetworkIds[0] != f.sourceNetworkId {
			t.Fatal("bounded registration continuation did not discover its newly registered payer", result, err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		withLegacyPayerQueueTestTx(ctx, []server.Id{f.sourceNetworkId}, func(tx server.PgTx) {
			for range 3 {
				ScheduleLegacyPayerSettlementsInTx(owner, tx, f.sourceNetworkId, nil, server.NowUtc())
			}
		})
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(NewLegacyPayerSettlementTaskTarget())
		var availableBlock int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT available_block FROM pending_task WHERE run_once_key=$1`,
				task.RunOnce("flush_legacy_payer_settlements", f.sourceNetworkId).String()).Scan(&availableBlock))
		})
		select {
		case <-time.After(time.Until(time.Unix(availableBlock, 0))):
		case <-ctx.Done():
			t.Fatal("payer owner did not become claim eligible", ctx.Err())
		}
		finished, retried, postRetried, err := worker.EvalTasks(4)
		if err != nil || len(finished) != 1 || len(retried)+len(postRetried) != 0 {
			t.Fatal("coalesced payer task did not execute exactly one financial turn", len(finished), err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
		server.Db(ctx, func(conn server.PgConn) {
			var remaining int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE run_once_key=$1`, task.RunOnce("flush_legacy_payer_settlements", f.sourceNetworkId).String()).Scan(&remaining))
			if remaining != 0 {
				t.Fatal("empty payer retained an unnecessary recurring task")
			}
		})
	})
}

// A full compatibility prefix commits as one protocol batch and resumes from
// its confirmed chronological cursor. The bound includes already registered
// rows; it cannot widen into an unbounded NULL scan when that index is absent.
func TestLegacyPayerDispatchRegistersFullBoundedNullPrefix(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		ids := make([]server.Id, legacySettlementPayerRegistrationLimit+1)
		const shard = 7
		prefix := server.NewId()
		for i := range ids {
			ids[i] = legacyPayerTestContractId(prefix, uint32(i+1), shard)
		}
		oldest := server.NowUtc().Add(-time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP INDEX legacy_settlement_intent_payer_missing`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
 SELECT id,$2,$3,$4,$5,$2,0,true FROM unnest($1::uuid[]) AS pending(id)`,
				ids, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
 SELECT id,(get_byte(uuid_send(id),15)%16)::smallint,'settled',$2::timestamp+ordinal*interval '1 microsecond'
 FROM unnest($1::uuid[]) WITH ORDINALITY AS pending(id,ordinal)`, ids, oldest))
			// The current INSERT trigger fills the key. Explicitly reproduce
			// retained pre-registration rows after that trigger has completed.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=NULL WHERE contract_id=ANY($1)`, ids))
			var missing int
			server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1) AND payer_network_id IS NULL`, ids).Scan(&missing))
			if missing != len(ids) {
				t.Fatal("registration fixture did not retain its complete NULL prefix", missing)
			}
		})
		first, err := DispatchLegacySettlementPayers(ctx, shard, nil, nil)
		if err != nil || first.RegistrationFailed || first.Registered != legacySettlementPayerRegistrationLimit ||
			first.Cursor == nil || first.Cursor.ContractId != ids[len(ids)-2] || len(first.PayerNetworkIds) != 0 || !first.More {
			t.Fatal("full NULL prefix did not commit its exact bounded registration", first, err)
		}
		second, err := DispatchLegacySettlementPayers(ctx, shard, first.Cursor, first.PayerCursor)
		if err != nil || second.RegistrationFailed || second.Registered != 1 || second.Cursor != nil ||
			len(second.PayerNetworkIds) != 1 || second.PayerNetworkIds[0] != f.sourceNetworkId || !second.More {
			t.Fatal("registration cursor did not preserve the final NULL intent", second, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var registered, untouched int
			server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1) AND payer_network_id=$2),
 (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome IS NULL)`,
				ids, f.sourceNetworkId).Scan(&registered, &untouched))
			if registered != len(ids) || untouched != len(ids) {
				t.Fatal("compatibility registration changed financial authority or lost custody", registered, untouched)
			}
		})
	})
}

// Own-budget expiry hands back only fully completed real payer probes. Parent
// cancellation at the identical seam remains an error and cannot claim success.
func TestLegacyPayerDispatchBudgetRetainsPrefixButParentCancellationFails(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const shard = 3
		first := newNetEscrowOrderingTestFixture(t, ctx)
		second := newNetEscrowOrderingTestFixture(t, ctx)
		prefix := server.NewId()
		newLegacyPayerTestIntent(t, ctx, first, legacyPayerTestContractId(prefix, 1, shard), 100, 11)
		newLegacyPayerTestIntent(t, ctx, second, legacyPayerTestContractId(prefix, 2, shard), 100, 11)
		payers := []server.Id{first.sourceNetworkId, second.sourceNetworkId}
		slices.SortFunc(payers, server.Id.Cmp)
		for _, parentCanceled := range []bool{false, true} {
			parent, parentCancel := context.WithCancel(ctx)
			bounded, stop := context.WithCancelCause(parent)
			calls := 0
			result, err := dispatchLegacySettlementPayersPage(parent, bounded, shard, nil, nil,
				func(callCtx context.Context, callShard int, cursor *LegacySettlementPayerCursor) (*server.Id, *LegacySettlementPosition) {
					calls++
					if calls == 2 {
						if parentCanceled {
							parentCancel()
						} else {
							stop(errLegacySettlementDispatchBudget)
						}
						server.Raise(callCtx.Err())
					}
					return nextLegacySettlementPayer(callCtx, callShard, cursor)
				}, registerLegacySettlementPayerPage)
			parentCancel()
			stop(nil)
			if result.Probes != 1 || len(result.PayerNetworkIds) != 1 || result.PayerNetworkIds[0] != payers[0] ||
				result.PayerCursor == nil || result.PayerCursor.After == nil || *result.PayerCursor.After != payers[0] {
				t.Fatal("interrupted probe advanced past unobserved payer work", result, err)
			}
			if parentCanceled {
				if err == nil || result.More {
					t.Fatal("parent cancellation was converted into successful dispatch")
				}
				continue
			}
			if err != nil || !result.More {
				t.Fatal("owned deadline lost a completed scheduling prefix", result, err)
			}
			resumed, err := DispatchLegacySettlementPayers(ctx, shard, result.Cursor, result.PayerCursor)
			if err != nil || len(resumed.PayerNetworkIds) != 1 || resumed.PayerNetworkIds[0] != payers[1] {
				t.Fatal("completed-prefix continuation repeated early payers and starved later work", resumed, err)
			}
		}
	})
}

// Transaction cleanup may return after the query's context expires. The ready
// prefix is already in custody before registration starts; only the parent's
// cancellation can prevent its handoff, and no registration cursor is advanced.
func TestLegacyPayerDispatchRegistrationDeadlineKeepsReadyPrefix(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		shard := int(id[15]) % 16
		after := &LegacySettlementCursor{NextAttemptTime: server.NowUtc().Add(-time.Hour), ContractId: server.NewId(), PassEndTime: server.NowUtc()}
		for _, scenario := range []struct{ parentCanceled, registrationRefusal bool }{
			{parentCanceled: false, registrationRefusal: false},
			{parentCanceled: false, registrationRefusal: true},
			{parentCanceled: true, registrationRefusal: false},
			{parentCanceled: true, registrationRefusal: true},
		} {
			parentCanceled := scenario.parentCanceled
			parent, parentCancel := context.WithCancel(ctx)
			bounded, stop := context.WithCancelCause(parent)
			probes := 0
			result, err := dispatchLegacySettlementPayersPage(parent, bounded, shard, after, nil,
				func(callCtx context.Context, callShard int, cursor *LegacySettlementPayerCursor) (*server.Id, *LegacySettlementPosition) {
					payer, position := nextLegacySettlementPayer(callCtx, callShard, cursor)
					if payer != nil {
						probes++
					}
					return payer, position
				}, func(callCtx context.Context, _ int, _ *LegacySettlementCursor) (*LegacySettlementCursor, int) {
					if probes != 1 {
						t.Fatal("optional registration ran before ready discovery completed")
					}
					if parentCanceled {
						parentCancel()
					} else {
						stop(errLegacySettlementDispatchBudget)
					}
					if scenario.registrationRefusal {
						server.Raise(&pgconn.PgError{Code: "55P03", Message: "synthetic optional registration refusal"})
					}
					server.Raise(callCtx.Err())
					return nil, 0
				})
			parentCancel()
			stop(nil)
			if result.Probes != 1 || len(result.PayerNetworkIds) != 1 || result.PayerNetworkIds[0] != f.sourceNetworkId ||
				result.Cursor != after || result.Registered != 0 || !result.RegistrationFailed {
				t.Fatal("expired registration lost prefix or advanced uncommitted cursor", result, err)
			}
			if parentCanceled {
				if err == nil || result.More {
					t.Fatal("parent cancellation during registration was converted to success", result, err)
				}
			} else if err != nil || !result.More {
				t.Fatal("registration cleanup expiry suppressed ready payer handoff", result, err)
			}
		}
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
	})
}

// A completed turn can have an old forward cursor. A newly committed older
// key must receive a reset continuation even if the current Run observes EOF.
func TestLegacyPayerTaskEofRechecksAndResetsPredecessor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		args := &LegacyPayerSettlementArgs{Private: true, PayerNetworkId: f.sourceNetworkId,
			Cursor: &LegacySettlementCursor{NextAttemptTime: server.NowUtc().Add(time.Hour), ContractId: server.NewId(), PassEndTime: server.NowUtc()}}
		result, err := ApplyLegacyPayerSettlements(args, owner)
		if err != nil || result.Completed != 0 || result.Cursor != nil {
			t.Fatal("synthetic stale forward cursor did not reach EOF", result, err)
		}
		withLegacyPayerQueueTestTx(ctx, []server.Id{f.sourceNetworkId}, func(tx server.PgTx) {
			server.Raise(ApplyLegacyPayerSettlementsPost(args, result, owner, tx))
		})
		var next LegacyPayerSettlementArgs
		server.Db(ctx, func(conn server.PgConn) {
			var data []byte
			server.Raise(conn.QueryRow(ctx, `SELECT args_json FROM pending_task WHERE run_once_key=$1`, task.RunOnce("flush_legacy_payer_settlements", f.sourceNetworkId).String()).Scan(&data))
			server.Raise(json.Unmarshal(data, &next))
		})
		if next.Cursor != nil || !next.Private || next.PayerNetworkId != f.sourceNetworkId {
			t.Fatal("EOF handoff retained a cursor that hides predecessor work")
		}
		result, err = ApplyLegacyPayerSettlements(&next, owner)
		if err != nil || result.Completed != 1 {
			t.Fatal("reset continuation did not settle its predecessor", result, err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
	})
}

// Scheduling is an independent task concern. A held payer queue row cannot
// make Close wait for another contract's task completion.
func TestLegacyPayerQueueRowIsAbsentFromForegroundClose(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		escrow, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		withLegacyPayerQueueTestTx(ctx, []server.Id{f.sourceNetworkId}, func(tx server.PgTx) {
			ScheduleLegacyPayerSettlementsInTx(owner, tx, f.sourceNetworkId, nil, server.NowUtc())
		})
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(ctx)
		server.RaisePgResult(held.Exec(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1 FOR UPDATE`, task.RunOnce("flush_legacy_payer_settlements", f.sourceNetworkId).String()))
		closeCtx, closeCancel := context.WithTimeout(ctx, 2*time.Second)
		defer closeCancel()
		server.Raise(CloseContract(closeCtx, escrow.ContractId, f.sourceId, 11, false))
		server.Raise(CloseContract(closeCtx, escrow.ContractId, f.destinationId, 11, false))
		requireLegacySettlementTestState(t, ctx, f, escrow.ContractId, true, false, 1000, 100)
	})
}

// Failed compatibility registration is optional scheduling work. Its rollback
// retains the old cursor and cannot consume the registered payer's service turn.
func TestLegacyPayerDispatchHeldRegistrationKeepsRegisteredService(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		missing, missingId := legacySettlementTestIntent(t, ctx)
		shard := int(missingId[15]) % 16
		ready := newNetEscrowOrderingTestFixture(t, ctx)
		readyId := newLegacyPayerTestIntent(t, ctx, ready, legacyPayerTestContractId(server.NewId(), 1, shard), 100, 11)
		server.Tx(ctx, func(tx server.PgTx) {
			// Exercise the chronological fallback's existing timeout custody.
			server.RaisePgResult(tx.Exec(ctx, `DROP INDEX legacy_settlement_intent_payer_missing`))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=NULL WHERE contract_id=$1`, missingId))
		})
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT contract_id FROM legacy_settlement_intent WHERE contract_id=$1 FOR UPDATE`, missingId))
		result, err := DispatchLegacySettlementPayers(ctx, shard, nil, nil)
		if err != nil || !result.RegistrationFailed || result.Cursor != nil || result.Registered != 0 || len(result.PayerNetworkIds) != 1 || result.PayerNetworkIds[0] != ready.sourceNetworkId {
			t.Fatal("failed registration parked independently registered work", result, err)
		}
		requireLegacySettlementTestState(t, ctx, missing, missingId, true, false, 1000, 100)
		requireLegacySettlementTestState(t, ctx, ready, readyId, true, false, 1000, 100)
		server.Raise(held.Rollback(ctx))
		result, err = DispatchLegacySettlementPayers(ctx, shard, nil, nil)
		if err != nil || result.RegistrationFailed || result.Registered != 1 || len(result.PayerNetworkIds) != 1 || !result.More {
			t.Fatal("released compatibility work was not rediscovered", result, err)
		}
		result, err = DispatchLegacySettlementPayers(ctx, shard, result.Cursor, result.PayerCursor)
		if err != nil || len(result.PayerNetworkIds) != 2 {
			t.Fatal("registered compatibility work lost its continuation", result, err)
		}
	})
}
