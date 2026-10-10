// Free expiry retains the public no-payout owner across dispute and restart.
package model

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Both routes share actual public no-escrow creation and the same endpoints.
type freeExpiryOwnerFixture struct {
	networkId     server.Id
	sourceId      server.Id
	destinationId server.Id
}

// Generated names keep repeated deadline cases independent in one database.
func newFreeExpiryOwnerFixture(ctx context.Context) freeExpiryOwnerFixture {
	f := freeExpiryOwnerFixture{networkId: server.NewId(), sourceId: server.NewId(), destinationId: server.NewId()}
	Testing_CreateNetwork(ctx, f.networkId, "synthetic-free-expiry-"+f.networkId.String(), server.NewId())
	Testing_CreateDevice(ctx, f.networkId, server.NewId(), f.sourceId, "synthetic-source", "synthetic")
	Testing_CreateDevice(ctx, f.networkId, server.NewId(), f.destinationId, "synthetic-destination", "synthetic")
	return f
}

// Clock ownership is inspected only after the real page has joined its posts.
func freeExpiryOwnerClockCount(batch *legacySettlementPostBatch) int {
	batch.stateLock.Lock()
	defer batch.stateLock.Unlock()
	for _, count := range batch.clockByteCounts {
		if count != 17 {
			return -1
		}
	}
	return len(batch.clockByteCounts)
}

// No free terminal path may create a reservation, payout, or debit journal.
func requireFreeExpiryOwnerClosed(t testing.TB, ctx context.Context, ids []server.Id) {
	t.Helper()
	requireFreeExpiryOwnerClosedReports(t, ctx, ids, 2, false)
}

// A forced one-sided close retains only its original source checkpoint; an
// ordinary bilateral close retains both final reports. Neither creates debt.
func requireFreeExpiryOwnerClosedReports(t testing.TB, ctx context.Context, ids []server.Id, reportsPerContract int, checkpoint bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var exact bool
		server.Raise(conn.QueryRow(ctx, `SELECT
            (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1::uuid[]) AND outcome='settled' AND NOT dispute AND NOT open)=$2
            AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($1::uuid[]))
            AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=ANY($1::uuid[]))
            AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=ANY($1::uuid[]))
            AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=ANY($1::uuid[]))
            AND (SELECT count(*)=$2*$3::bigint AND bool_and(checkpoint=$4 AND used_transfer_byte_count=17 AND ($3::bigint<>1 OR party='source'))
                FROM contract_close WHERE contract_id=ANY($1::uuid[]))`, ids, len(ids), reportsPerContract, checkpoint).Scan(&exact))
		if !exact {
			t.Fatal("free expiry lost final positive reports or acquired financial custody")
		}
	})
}

// Only creation/deadline clocks move. A recent authentic checkpoint remains
// recent, proving the hard deadline rather than a quiet-report shortcut.
func expireFreeExpiryOwnerContracts(ctx context.Context, ids []server.Id, missingDeadline bool) {
	now := server.NowUtc()
	var deadline *time.Time
	if !missingDeadline {
		past := now.Add(-time.Minute)
		deadline = &past
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=$3 WHERE contract_id=ANY($1::uuid[])`,
			ids, now.Add(-61*time.Minute), deadline))
	})
}

// Public dispute creation permits a positive free checkpoint. The existing
// source-intent owner is the matched control for ordinary no-intent expiry.
func TestContractExpirationFreeDisputeUsesPublicSourceOwner(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		batch := &legacySettlementPostBatch{mirrorBalanceIdSet: map[server.Id]bool{}}
		ctx = context.WithValue(ctx, legacySettlementPostBatchKey{}, batch)
		server.Redis(ctx, func(r server.RedisClient) { server.Raise(r.Del(ctx, clockTransferByteCountRedisKey).Err()) })
		for index, missingDeadline := range []bool{false, true} {
			f := newFreeExpiryOwnerFixture(ctx)
			ids := make([]server.Id, 2)
			for i := range ids {
				id, err := CreateContractNoEscrow(ctx, f.networkId, f.sourceId, f.networkId, f.destinationId, 100)
				server.Raise(err)
				server.Raise(CloseContract(ctx, id, f.sourceId, 17, true))
				SetContractDispute(ctx, id, true)
				ids[i] = id
			}
			if missingDeadline {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL WHERE contract_id=ANY($1::uuid[])`, ids))
				})
			}
			cutoff := server.NowUtc().Add(-5 * time.Minute)
			before := [][]byte{readRedisExpiryRepairTestState(ctx, ids[0]), readRedisExpiryRepairTestState(ctx, ids[1])}
			count, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
			if err != nil || count != 0 {
				t.Fatal("fresh positive free dispute was selected", missingDeadline, count, err)
			}
			for i, id := range ids {
				if !bytes.Equal(before[i], readRedisExpiryRepairTestState(ctx, id)) {
					t.Fatal("fresh free dispute changed before its deadline")
				}
			}
			expireFreeExpiryOwnerContracts(ctx, ids, missingDeadline)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, ids[1]))
				server.Raise(queueLegacySettlementInTx(ctx, tx, ids[1], ContractOutcomeSettled, true))
			})
			owned := context.WithValue(ctx, legacySettlementCloseScopeKey{}, ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: f.sourceId})
			beforeClosed := contractClosedCounter.Snapshot()
			completed, busy, _, err := flushLegacySettlement(owned, ids[1])
			if err != nil || !completed || busy || freeExpiryOwnerClockCount(batch) != index*2+1 {
				t.Fatal("matched source-intent control lost positive free usage", completed, busy, err)
			}
			count, _, err = ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
			if err != nil || count != 1 || freeExpiryOwnerClockCount(batch) != index*2+2 {
				t.Fatal("ordinary free dispute did not use its no-payout owner", missingDeadline, count, err)
			}
			if missingDeadline {
				requireFreeExpiryOwnerClosed(t, ctx, ids)
			} else {
				requireFreeExpiryOwnerClosedReports(t, ctx, ids, 1, true)
			}
			proof, snapshot := readContractExpiryTestSnapshot(t, ctx, ids[0])
			controlProof, _ := readContractExpiryTestSnapshot(t, ctx, ids[1])
			if !bytes.Equal(proof, controlProof) || snapshot.ByteCount != 0 || len(snapshot.Providers) != 0 ||
				snapshot.ExcludedReason != "expired_unconfirmed" || snapshot.Expiry == nil || len(snapshot.Expiry.Reports) != 1 ||
				snapshot.Expiry.Reports[ContractPartySource].ByteCount != 17 || !snapshot.Expiry.Reports[ContractPartySource].Checkpoint {
				t.Fatal("free dispute invented a peer in its immutable usage proof")
			}
			afterClosed := contractClosedCounter.Snapshot()
			if !beforeClosed.Stable || !afterClosed.Stable || afterClosed.Confirmed-beforeClosed.Confirmed != 2 ||
				afterClosed.Uncertain != beforeClosed.Uncertain || afterClosed.Untracked != beforeClosed.Untracked {
				t.Fatal("free dispute changed confirmed outcome counting", beforeClosed, afterClosed)
			}
			before = [][]byte{readRedisExpiryRepairTestState(ctx, ids[0]), readRedisExpiryRepairTestState(ctx, ids[1])}
			count, _, err = ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
			if err != nil || count != 0 || freeExpiryOwnerClockCount(batch) != index*2+2 {
				t.Fatal("terminal free dispute replay repeated its outcome or clock", count, err)
			}
			for i, id := range ids {
				if !bytes.Equal(before[i], readRedisExpiryRepairTestState(ctx, id)) {
					t.Fatal("terminal free dispute replay changed retained state")
				}
			}
		}
		batch.finish(ctx)
		clock, ok := GetClock(ctx)
		if !ok || clock.TotalTransferByteCount != "68" {
			t.Fatal("free dispute owners did not publish exactly four positive destination clocks", clock, ok)
		}
	})
}

// A trigger refuses only the outcome transaction; the public report owner
// has already committed the destination report before that boundary.
func interruptFreeExpiryPublicClose(t testing.TB, ctx context.Context, id, destinationId server.Id) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_free_outcome_abort() RETURNS trigger LANGUAGE plpgsql AS $$
                    BEGIN RAISE EXCEPTION USING ERRCODE='57014',MESSAGE='synthetic free outcome interruption'; END;
                    $$;
                    CREATE TRIGGER synthetic_free_outcome_abort BEFORE UPDATE ON transfer_contract
                    FOR EACH ROW WHEN (OLD.outcome IS NULL AND NEW.outcome IS NOT NULL)
                    EXECUTE FUNCTION synthetic_free_outcome_abort();`))
	})
	var interrupted error
	server.HandleError(func() { interrupted = CloseContract(ctx, id, destinationId, 17, false) },
		func(err error) { interrupted = err })
	if interrupted == nil {
		t.Fatal("public close did not stop after the final report commit")
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_free_outcome_abort ON transfer_contract; DROP FUNCTION synthetic_free_outcome_abort();`))
	})
	server.Db(ctx, func(conn server.PgConn) {
		var interruptedState bool
		server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND NOT dispute AND open AND NOT usage_unverified AND provider_usage IS NULL
                    AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
                    AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1)
                    AND (SELECT count(*)=2 AND bool_and(NOT checkpoint AND used_transfer_byte_count=17)
                        FROM contract_close WHERE contract_id=$1)
                    FROM transfer_contract WHERE contract_id=$1`, id).Scan(&interruptedState))
		if !interruptedState {
			t.Fatal("public interruption did not retain both final reports without settlement custody")
		}
	})
}

// The real public report commits before its outcome transaction. Refusing that
// outcome deterministically leaves two final reports without an intent; expiry
// must recover the same free close as an uninterrupted public-close control.
func TestContractExpirationFreeFinalReportsResumePublicClose(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		batch := &legacySettlementPostBatch{mirrorBalanceIdSet: map[server.Id]bool{}}
		ctx = context.WithValue(ctx, legacySettlementPostBatchKey{}, batch)
		server.Redis(ctx, func(r server.RedisClient) { server.Raise(r.Del(ctx, clockTransferByteCountRedisKey).Err()) })
		for index, missingDeadline := range []bool{false, true} {
			f := newFreeExpiryOwnerFixture(ctx)
			ids := make([]server.Id, 2)
			for i := range ids {
				id, err := CreateContractNoEscrow(ctx, f.networkId, f.sourceId, f.networkId, f.destinationId, 100)
				server.Raise(err)
				server.Raise(CloseContract(ctx, id, f.sourceId, 17, false))
				ids[i] = id
			}
			server.Raise(CloseContract(ctx, ids[1], f.destinationId, 17, false))
			interruptFreeExpiryPublicClose(t, ctx, ids[0], f.destinationId)
			expireFreeExpiryOwnerContracts(ctx, ids[:1], missingDeadline)
			beforeClosed := contractClosedCounter.Snapshot()
			count, _, err := ForceCloseOpenContractIdsPage(ctx, server.NowUtc().Add(-5*time.Minute), 32, 1, 0, 0, nil)
			if err != nil || count != 1 || freeExpiryOwnerClockCount(batch) != index+1 {
				t.Fatal("interrupted free close entered accounting rejection or quarantine", missingDeadline, count, err)
			}
			requireFreeExpiryOwnerClosed(t, ctx, ids)
			proof, snapshot := readContractExpiryTestSnapshot(t, ctx, ids[0])
			_, control := readContractExpiryTestSnapshot(t, ctx, ids[1])
			if snapshot.ByteCount != 17 || snapshot.ByteCount != control.ByteCount || len(snapshot.Providers) != 1 || len(control.Providers) != 1 ||
				snapshot.Providers[0] != control.Providers[0] || snapshot.Providers[0].ClientId != f.destinationId ||
				snapshot.Providers[0].ByteCount != 17 || snapshot.Expiry == nil || len(snapshot.Expiry.Reports) != 2 {
				t.Fatal("interrupted free close changed authenticated usage from its public control")
			}
			for _, party := range []ContractParty{ContractPartySource, ContractPartyDestination} {
				report, found := snapshot.Expiry.Reports[party]
				if !found || report.Checkpoint || report.ByteCount != 17 {
					t.Fatal("interrupted close proof did not retain its authentic final reports")
				}
			}
			afterClosed := contractClosedCounter.Snapshot()
			if !beforeClosed.Stable || !afterClosed.Stable || afterClosed.Confirmed-beforeClosed.Confirmed != 1 ||
				afterClosed.Uncertain != beforeClosed.Uncertain || afterClosed.Untracked != beforeClosed.Untracked {
				t.Fatal("interrupted free close did not count one confirmed outcome", beforeClosed, afterClosed)
			}
			before := readRedisExpiryRepairTestState(ctx, ids[0])
			count, _, err = ForceCloseOpenContractIdsPage(ctx, server.NowUtc().Add(-5*time.Minute), 32, 1, 0, 0, nil)
			afterProof, _ := readContractExpiryTestSnapshot(t, ctx, ids[0])
			if err != nil || count != 0 || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, ids[0])) ||
				!bytes.Equal(proof, afterProof) || freeExpiryOwnerClockCount(batch) != index+1 {
				t.Fatal("interrupted free close replay changed proof, state, or destination clock", count, err)
			}
		}
		batch.finish(ctx)
		clock, ok := GetClock(ctx)
		if !ok || clock.TotalTransferByteCount != "68" {
			t.Fatal("resumed and uninterrupted public closes did not each clock seventeen bytes once", clock, ok)
		}
	})
}

// A canceled dispute clear must roll back its synthesized peer and checkpoint
// finalization. Its previously committed original proof remains the retry owner.
func TestContractExpirationFreeDisputeCancellationKeepsProofAndReports(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		batch := &legacySettlementPostBatch{mirrorBalanceIdSet: map[server.Id]bool{}}
		ctx = context.WithValue(ctx, legacySettlementPostBatchKey{}, batch)
		server.Redis(ctx, func(r server.RedisClient) { server.Raise(r.Del(ctx, clockTransferByteCountRedisKey).Err()) })
		f := newFreeExpiryOwnerFixture(ctx)
		id, err := CreateContractNoEscrow(ctx, f.networkId, f.sourceId, f.networkId, f.destinationId, 100)
		server.Raise(err)
		server.Raise(CloseContract(ctx, id, f.sourceId, 17, true))
		SetContractDispute(ctx, id, true)
		expireFreeExpiryOwnerContracts(ctx, []server.Id{id}, true)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_free_dispute_cancel() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN RAISE EXCEPTION USING ERRCODE='57014',MESSAGE='synthetic free dispute cancellation'; END;
                $$;
                CREATE TRIGGER synthetic_free_dispute_cancel AFTER UPDATE ON transfer_contract
                FOR EACH ROW WHEN (OLD.dispute AND NOT NEW.dispute AND NEW.outcome IS NULL)
                EXECUTE FUNCTION synthetic_free_dispute_cancel();`))
		})
		beforeClosed := contractClosedCounter.Snapshot()
		count, _, err := ForceCloseOpenContractIdsPage(ctx, server.NowUtc().Add(-5*time.Minute), 32, 1, 0, 0, nil)
		var canceled *pgconn.PgError
		if count != 1 || !errors.As(err, &canceled) || canceled.Code != "57014" || freeExpiryOwnerClockCount(batch) != 0 {
			t.Fatal("free dispute cancellation lost its refusal or published a clock", count, err)
		}
		afterFailure := contractClosedCounter.Snapshot()
		if !beforeClosed.Stable || !afterFailure.Stable || afterFailure.Confirmed != beforeClosed.Confirmed ||
			afterFailure.Uncertain != beforeClosed.Uncertain || afterFailure.Untracked != beforeClosed.Untracked {
			t.Fatal("canceled free dispute counted an attempted row as a confirmed outcome", beforeClosed, afterFailure)
		}
		proof, snapshot := readContractExpiryTestSnapshot(t, ctx, id)
		if snapshot.ByteCount != 0 || snapshot.Expiry == nil || len(snapshot.Expiry.Reports) != 1 ||
			snapshot.Expiry.Reports[ContractPartySource].ByteCount != 17 || !snapshot.Expiry.Reports[ContractPartySource].Checkpoint {
			t.Fatal("canceled free dispute lost its original partial proof")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var retained bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND dispute AND usage_unverified
                AND (SELECT count(*)=1 AND bool_and(party='source' AND checkpoint AND used_transfer_byte_count=17)
                    FROM contract_close WHERE contract_id=$1)
                AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
                AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1)
                AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
                AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
                FROM transfer_contract WHERE contract_id=$1`, id).Scan(&retained))
			if !retained {
				t.Fatal("canceled free dispute leaked a peer, outcome, or financial write")
			}
		})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_free_dispute_cancel ON transfer_contract`))
		})
		count, _, err = ForceCloseOpenContractIdsPage(ctx, server.NowUtc().Add(-5*time.Minute), 32, 1, 0, 0, nil)
		afterProof, _ := readContractExpiryTestSnapshot(t, ctx, id)
		if err != nil || count != 1 || !bytes.Equal(proof, afterProof) || freeExpiryOwnerClockCount(batch) != 1 {
			t.Fatal("free dispute did not resume its exact retained proof", count, err)
		}
		requireFreeExpiryOwnerClosed(t, ctx, []server.Id{id})
		afterClosed := contractClosedCounter.Snapshot()
		if !beforeClosed.Stable || !afterClosed.Stable || afterClosed.Confirmed-beforeClosed.Confirmed != 1 ||
			afterClosed.Uncertain != beforeClosed.Uncertain || afterClosed.Untracked != beforeClosed.Untracked {
			t.Fatal("canceled free dispute counted an uncommitted outcome", beforeClosed, afterClosed)
		}
		count, _, err = ForceCloseOpenContractIdsPage(ctx, server.NowUtc().Add(-5*time.Minute), 32, 1, 0, 0, nil)
		if err != nil || count != 0 || freeExpiryOwnerClockCount(batch) != 1 {
			t.Fatal("resumed free dispute replay repeated its clock", count, err)
		}
		batch.finish(ctx)
		clock, ok := GetClock(ctx)
		if !ok || clock.TotalTransferByteCount != "17" {
			t.Fatal("resumed free dispute did not publish exactly one positive clock", clock, ok)
		}
	})
}

// An accepted intent can appear after page preparation commits. Ordinary
// expiry must hand it to the dispatcher without consuming or changing its
// selected outcome, including when the original candidate was disputed.
func TestContractExpirationFreeContinuationDefersNewAcceptedIntent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		batch := &legacySettlementPostBatch{mirrorBalanceIdSet: map[server.Id]bool{}}
		ctx = context.WithValue(ctx, legacySettlementPostBatchKey{}, batch)
		for _, disputed := range []bool{false, true} {
			f := newFreeExpiryOwnerFixture(ctx)
			id, err := CreateContractNoEscrow(ctx, f.networkId, f.sourceId, f.networkId, f.destinationId, 100)
			server.Raise(err)
			server.Raise(CloseContract(ctx, id, f.sourceId, 17, disputed))
			if disputed {
				SetContractDispute(ctx, id, true)
			} else {
				interruptFreeExpiryPublicClose(t, ctx, id, f.destinationId)
			}
			expireFreeExpiryOwnerContracts(ctx, []server.Id{id}, true)
			readIntent := func() string {
				var state string
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT row_to_json(i)::text FROM legacy_settlement_intent i WHERE contract_id=$1`, id).Scan(&state))
				})
				return state
			}
			var before []byte
			var beforeIntent string
			arrivals := 0
			callCtx := context.WithValue(ctx, forceCloseContinuationContextKey{}, func(parent context.Context, selected server.Id) context.Context {
				if selected != id {
					return parent
				}
				arrivals++
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, id))
					server.Raise(queueLegacySettlementInTx(ctx, tx, id, ContractOutcomeDisputeResolvedToSource, true))
				})
				before, beforeIntent = readRedisExpiryRepairTestState(ctx, id), readIntent()
				return parent
			})
			beforeClosed := contractClosedCounter.Snapshot()
			count, _, err := ForceCloseOpenContractIdsPage(callCtx, server.NowUtc().Add(-5*time.Minute), 32, 1, 0, 0, nil)
			if err != nil || count != 0 || arrivals != 1 || len(before) == 0 || beforeIntent == "" ||
				!bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) || beforeIntent != readIntent() || freeExpiryOwnerClockCount(batch) != 0 {
				t.Fatal("free expiry did not defer the new accepted intent unchanged", disputed, count, err)
			}
			afterClosed := contractClosedCounter.Snapshot()
			if !beforeClosed.Stable || !afterClosed.Stable || afterClosed.Confirmed != beforeClosed.Confirmed ||
				afterClosed.Uncertain != beforeClosed.Uncertain || afterClosed.Untracked != beforeClosed.Untracked {
				t.Fatal("accepted-intent handoff counted a completed free contract", beforeClosed, afterClosed)
			}
		}
	})
}

// Public dispute creation between proof and continuation changes the current
// owner state. The no-edit branch must clear that actual dispute atomically.
func TestContractExpirationFreeFinalContinuationReloadsPublicDispute(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		batch := &legacySettlementPostBatch{mirrorBalanceIdSet: map[server.Id]bool{}}
		ctx = context.WithValue(ctx, legacySettlementPostBatchKey{}, batch)
		f := newFreeExpiryOwnerFixture(ctx)
		id, err := CreateContractNoEscrow(ctx, f.networkId, f.sourceId, f.networkId, f.destinationId, 100)
		server.Raise(err)
		server.Raise(CloseContract(ctx, id, f.sourceId, 17, false))
		interruptFreeExpiryPublicClose(t, ctx, id, f.destinationId)
		expireFreeExpiryOwnerContracts(ctx, []server.Id{id}, true)
		var proof []byte
		arrivals := 0
		callCtx := context.WithValue(ctx, forceCloseContinuationContextKey{}, func(parent context.Context, selected server.Id) context.Context {
			if selected == id {
				arrivals++
				proof, _ = readContractExpiryTestSnapshot(t, ctx, id)
				SetContractDispute(ctx, id, true)
			}
			return parent
		})
		count, _, err := ForceCloseOpenContractIdsPage(callCtx, server.NowUtc().Add(-5*time.Minute), 32, 1, 0, 0, nil)
		afterProof, _ := readContractExpiryTestSnapshot(t, ctx, id)
		if err != nil || count != 1 || arrivals != 1 || !bytes.Equal(proof, afterProof) || freeExpiryOwnerClockCount(batch) != 1 {
			t.Fatal("free no-edit continuation used stale dispute or report ownership", count, err)
		}
		requireFreeExpiryOwnerClosed(t, ctx, []server.Id{id})
		count, _, err = ForceCloseOpenContractIdsPage(ctx, server.NowUtc().Add(-5*time.Minute), 32, 1, 0, 0, nil)
		if err != nil || count != 0 || freeExpiryOwnerClockCount(batch) != 1 {
			t.Fatal("newly disputed free continuation repeated its terminal clock", count, err)
		}
	})
}

// A retained proof never exempts current reports from validation. Exercise the
// continuation itself so the sweep's separate malformed-quarantine policy is
// not confused with authority to claim a normal successful free outcome.
func TestContractExpirationFreeFinalContinuationRejectsChangedReport(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		batch := &legacySettlementPostBatch{mirrorBalanceIdSet: map[server.Id]bool{}}
		ctx = context.WithValue(ctx, legacySettlementPostBatchKey{}, batch)
		f := newFreeExpiryOwnerFixture(ctx)
		id, err := CreateContractNoEscrow(ctx, f.networkId, f.sourceId, f.networkId, f.destinationId, 100)
		server.Raise(err)
		server.Raise(CloseContract(ctx, id, f.sourceId, 17, false))
		interruptFreeExpiryPublicClose(t, ctx, id, f.destinationId)
		expireFreeExpiryOwnerContracts(ctx, []server.Id{id}, true)
		var prepared *contractExpiryState
		server.Tx(ctx, func(tx server.PgTx) {
			prepared, err = prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-5*time.Minute))
			server.Raise(err)
		})
		if prepared == nil || !prepared.usageUnverifiedRetained {
			t.Fatal("changed-report fixture lacks its original expiry proof")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=-1 WHERE contract_id=$1 AND party='destination'`, id))
		})
		before := readRedisExpiryRepairTestState(ctx, id)
		err = captureContractExpiryRepair(func() error { return continueContractExpiry(ctx, "synthetic-free-expiry", prepared, nil) })
		if err == nil || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) || freeExpiryOwnerClockCount(batch) != 0 {
			t.Fatal("free continuation used retained proof to bypass malformed current reports", err)
		}
	})
}
