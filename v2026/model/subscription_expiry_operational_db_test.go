// Native checkpoint failures must preserve custody until the original owner recovers.
package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

const forceCloseOperationalUsage = ByteCount(1024)
const forceCloseOperationalEscrow = ByteCount(4096)

func newForceCloseOperationalFixture(t testing.TB, ctx context.Context, legacy bool) *forceCloseDisputeFixture {
	t.Helper()
	f := newForceCloseDisputeFixtureWithAdmission(t, ctx, true, true,
		forceCloseOperationalUsage, forceCloseOperationalUsage, forceCloseOperationalEscrow, legacy)
	// The existing provider share makes one used byte exactly one earned nano
	// cent, so recovery must preserve positive money as well as reservation bytes.
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=$2 WHERE balance_id=$1`,
			f.balanceId, 2*int64(forceCloseDisputeInitialBalance)))
	})
	before := f.state(t, ctx)
	if !before.open || before.outcome != "" || before.dispute || before.escrowSettled || !before.streamFound ||
		!before.sourceCheckpoint || !before.destinationCheckpoint || before.netEscrowByteCount != forceCloseOperationalEscrow ||
		before.payerBalanceByteCount != forceCloseDisputeInitialBalance || before.providerEarnedByteCount != 0 ||
		before.redisReserved == legacy {
		t.Fatal("operational fixture did not start with healthy reserved checkpoints", before)
	}
	return f
}

func forceCloseCheckpointFailure(ctx context.Context, code string) func() {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
			CREATE SEQUENCE synthetic_checkpoint_failure_attempts;
			CREATE FUNCTION synthetic_checkpoint_failure() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				PERFORM nextval('synthetic_checkpoint_failure_attempts');
				RAISE EXCEPTION USING ERRCODE='%s', MESSAGE='synthetic checkpoint failure';
			END;
			$$;
			CREATE TRIGGER synthetic_checkpoint_failure
			BEFORE UPDATE OF checkpoint ON contract_close
			FOR EACH ROW WHEN (OLD.checkpoint AND NOT NEW.checkpoint)
			EXECUTE FUNCTION synthetic_checkpoint_failure();`, code)))
	})
	return func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER IF EXISTS synthetic_checkpoint_failure ON contract_close`))
		})
	}
}

func requireForceCloseOperationalPreserved(t testing.TB, ctx context.Context, f *forceCloseDisputeFixture,
	before forceCloseDisputeState, err error,
) []byte {
	t.Helper()
	var accounting *ForceCloseAccountingError
	if err == nil || errors.As(err, &accounting) {
		t.Fatal("operational failure disappeared or gained accounting progress authority", err)
	}
	if after := f.state(t, ctx); after != before {
		t.Fatalf("operational checkpoint failure quarantined or changed healthy custody: before=%+v after=%+v", before, after)
	}
	var newOwners int64
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=$1) +
			(SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=$1) +
			(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=$1)`, f.contractId).Scan(&newOwners))
	})
	if newOwners != 0 || readForceCloseProviderProjection(t, ctx, f) != (forceCloseProviderProjection{}) {
		t.Fatal("failed checkpoint created a debit, settlement, or payout owner")
	}
	proof, snapshot := readContractExpiryTestSnapshot(t, ctx, f.contractId)
	if snapshot.Expiry == nil || len(snapshot.Expiry.Reports) != 2 || snapshot.ByteCount != forceCloseOperationalUsage ||
		!snapshot.Expiry.Reports[ContractPartySource].Checkpoint || !snapshot.Expiry.Reports[ContractPartyDestination].Checkpoint ||
		snapshot.Expiry.Reports[ContractPartySource].ByteCount != forceCloseOperationalUsage ||
		snapshot.Expiry.Reports[ContractPartyDestination].ByteCount != forceCloseOperationalUsage {
		t.Fatal("failed continuation lost the committed original checkpoint proof")
	}
	return proof
}

func requireForceCloseOperationalRecovery(t testing.TB, ctx context.Context, f *forceCloseDisputeFixture, legacy bool, proof []byte) {
	t.Helper()
	count, err := ForceCloseOpenContractIds(ctx, f.cutoff, 10, 1, 0, 0)
	wantCount := int64(1)
	if legacy {
		wantCount = 0 // The durable legacy owner still owns this financial close.
	}
	if err != nil || count != wantCount {
		t.Fatal("healthy recovery failed to reach its ordinary financial owner", count, err)
	}
	retained, snapshot := readContractExpiryTestSnapshot(t, ctx, f.contractId)
	if snapshot.ByteCount != forceCloseOperationalUsage || snapshot.Expiry == nil || len(snapshot.Expiry.Reports) != 2 {
		t.Fatal("healthy continuation did not retain original bilateral usage")
	}
	if proof == nil {
		// The fault-free control observes proof at the same pre-worker
		// boundary. Faulted cases supply the proof retained before recovery.
		proof = retained
	} else if !bytes.Equal(proof, retained) {
		t.Fatal("healthy continuation replaced the failed attempt's original proof")
	}
	if legacy {
		complete, busy, _, err := flushLegacySettlement(ctx, f.contractId)
		if err != nil || !complete || busy {
			t.Fatal("legacy recovery failed to apply its exact settlement", complete, busy, err)
		}
	} else {
		pending := f.state(t, ctx)
		// Foreground outcome, immutable consumption, and earnings commit
		// together. Only the debit worker owns escrow metadata and release;
		// the original whole reservation remains until that worker commits.
		if pending.outcome != ContractOutcomeSettled || pending.open || pending.dispute || pending.streamFound ||
			pending.escrowSettled || pending.escrowPayoutByteCount != 0 ||
			pending.sourceCheckpoint || pending.destinationCheckpoint || !pending.redisReserved ||
			pending.sourceByteCount != forceCloseOperationalUsage || pending.destinationByteCount != forceCloseOperationalUsage ||
			pending.providerEarnedByteCount != forceCloseOperationalUsage || pending.legacyEscrowByteCount != 0 ||
			pending.payerBalanceByteCount != forceCloseDisputeInitialBalance ||
			pending.netEscrowByteCount != forceCloseOperationalEscrow || pending.redisEscrowByteCount != forceCloseOperationalEscrow ||
			pending.requestTokenByteCount != forceCloseOperationalEscrow {
			t.Fatal("Redis recovery lost or prematurely released its durable pending debit", pending)
		}
		requireForceCloseDebitJournal(t, ctx, f, 1, forceCloseOperationalUsage)
		drainForceCloseDebitCustody(t, ctx, f, forceCloseOperationalEscrow, forceCloseOperationalUsage)
	}
	requireForceCloseDebitJournal(t, ctx, f, 0, 0)
	settled := f.state(t, ctx)
	if settled.outcome != ContractOutcomeSettled || settled.open || settled.dispute || settled.streamFound ||
		!settled.escrowSettled || settled.sourceCheckpoint || settled.destinationCheckpoint ||
		settled.sourceByteCount != forceCloseOperationalUsage || settled.destinationByteCount != forceCloseOperationalUsage ||
		settled.escrowPayoutByteCount != forceCloseOperationalUsage || settled.providerEarnedByteCount != forceCloseOperationalUsage ||
		settled.payerBalanceByteCount != forceCloseDisputeInitialBalance-forceCloseOperationalUsage ||
		settled.netEscrowByteCount != 0 || settled.legacyEscrowByteCount != 0 || settled.redisEscrowByteCount != 0 || settled.requestTokenByteCount != 0 {
		t.Fatal("recovery changed exact financial settlement", settled)
	}
	requireForceCloseAvailableCredit(t, ctx, f, forceCloseDisputeInitialBalance-forceCloseOperationalUsage)
	wantProjection := forceCloseProviderProjection{sweptBytes: forceCloseOperationalUsage,
		sweptRevenue: NanoCents(forceCloseOperationalUsage), unappliedBytes: forceCloseOperationalUsage,
		unappliedRevenue: NanoCents(forceCloseOperationalUsage), owners: 1}
	if got := readForceCloseProviderProjection(t, ctx, f); got != wantProjection {
		t.Fatalf("recovery lost exact durable money or payout ownership: got=%+v want=%+v", got, wantProjection)
	}
	target := task.NewTaskTarget(ApplyLegacyProviderTotals)
	var taskId server.Id
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE function_name=$1 AND run_once_key=$2`,
			target.TargetFunctionName(), task.RunOnce("legacy_provider_totals", f.contractId).String()).Scan(&taskId))
	})
	stale := task.GetTasks(ctx, taskId)[taskId]
	if stale == nil {
		t.Fatal("recovery lost its provider task")
	}
	for range 2 {
		// Repeat the same pre-application arguments to model a lost apply reply.
		if _, _, err := target.RunSpecific(ctx, stale); err != nil {
			t.Fatal("provider application or stale retry failed", err)
		}
		wantProjection = forceCloseProviderProjection{sweptBytes: forceCloseOperationalUsage,
			sweptRevenue: NanoCents(forceCloseOperationalUsage), accountBytes: forceCloseOperationalUsage,
			accountRevenue: NanoCents(forceCloseOperationalUsage), owners: 1, appliedOwners: 1}
		if got := readForceCloseProviderProjection(t, ctx, f); got != wantProjection {
			t.Fatalf("provider replay duplicated or lost bytes or money: got=%+v want=%+v", got, wantProjection)
		}
		if count, err := ForceCloseOpenContractIds(ctx, f.cutoff, 10, 1, 0, 0); err != nil || count != 0 {
			t.Fatal("terminal expiry replay repeated financial work", count, err)
		}
		if legacy {
			complete, busy, gate, err := flushLegacySettlement(ctx, f.contractId)
			if err != nil || complete || !busy || gate != legacySettlementBusyIntent {
				t.Fatal("legacy replay lost its deleted-intent disposition", complete, busy, gate, err)
			}
		} else {
			applied, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
			if err != nil || applied != 0 || released != 0 || busy {
				t.Fatal("debit replay repeated accounting", applied, released, busy, err)
			}
		}
		requireForceCloseDebitJournal(t, ctx, f, 0, 0)
		var intent bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)`, f.contractId).Scan(&intent))
		})
		if intent {
			t.Fatal("terminal replay restored a deleted settlement intent")
		}
		afterProof, _ := readContractExpiryTestSnapshot(t, ctx, f.contractId)
		if f.state(t, ctx) != settled || !bytes.Equal(proof, afterProof) {
			t.Fatal("recovery or replay changed original proof, financial totals, or terminal state")
		}
	}
}

// The journal is the consumption authority while metadata and the original
// reservation still belong to the debit worker. Count the exact contract,
// funding balance, shard, amount and applied state before/after that owner.
func requireForceCloseDebitJournal(t testing.TB, ctx context.Context, f *forceCloseDisputeFixture, wantCount int, wantBytes ByteCount) {
	t.Helper()
	var count, pending, applied int
	var amount ByteCount
	var exactOwner bool
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE NOT applied),count(*) FILTER(WHERE applied),
			COALESCE(sum(debit_byte_count),0),COALESCE(bool_and(balance_id=$2 AND shard=$3),true)
			FROM transfer_debit_journal WHERE contract_id=$1`, f.contractId, f.balanceId, transferDebitShard(f.balanceId)).
			Scan(&count, &pending, &applied, &amount, &exactOwner))
	})
	if count != wantCount || pending != wantCount || applied != 0 || amount != wantBytes || !exactOwner {
		t.Fatal("durable debit identity or amount differs from its actual worker boundary", count, pending, applied, amount, exactOwner)
	}
}

func requireForceCloseAvailableCredit(t testing.TB, ctx context.Context, f *forceCloseDisputeFixture, want ByteCount) server.Id {
	t.Helper()
	var networkId server.Id
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT network_id FROM transfer_balance WHERE balance_id=$1`, f.balanceId).Scan(&networkId))
	})
	if got := GetActiveTransferBalanceByteCount(ctx, networkId); got != want {
		t.Fatal("available credit differs from the debit owner's retained reservation", got, want)
	}
	return networkId
}

// Exercise the real public debit page, retaining its exact cursor and provider
// invariants. The unused reservation becomes available only after debit commit.
func drainForceCloseDebitCustody(t testing.TB, ctx context.Context, f *forceCloseDisputeFixture, reserved, consumed ByteCount) {
	t.Helper()
	requireForceCloseDebitJournal(t, ctx, f, 1, consumed)
	networkId := requireForceCloseAvailableCredit(t, ctx, f, forceCloseDisputeInitialBalance-reserved)
	payerAccount := contractPayoutTestAccountAmount(t, ctx, networkId)
	provider := readForceCloseProviderProjection(t, ctx, f)
	previous := f.balanceId
	for index := len(previous) - 1; ; index-- {
		if index < 0 {
			t.Fatal("synthetic balance has no preceding UUID")
		}
		if previous[index] != 0 {
			previous[index]--
			break
		}
		previous[index] = 255
	}
	result, err := FlushTransferDebits(ctx, transferDebitShard(f.balanceId), &previous, 1)
	if err != nil || result.Failed != 0 || result.Busy != 0 || result.Balances != 1 || !result.More ||
		result.Applied != 1 || result.Released != 1 || result.LastBalanceId == nil || *result.LastBalanceId != f.balanceId {
		t.Fatalf("public debit page lost its exact owner, consumption or cursor: %+v %v", result, err)
	}
	requireForceCloseDebitJournal(t, ctx, f, 0, 0)
	requireForceCloseAvailableCredit(t, ctx, f, forceCloseDisputeInitialBalance-consumed)
	settled := f.state(t, ctx)
	applied, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
	if err != nil || applied != 0 || released != 0 || busy {
		t.Fatal("empty debit replay repeated accounting", applied, released, busy, err)
	}
	requireForceCloseDebitJournal(t, ctx, f, 0, 0)
	if f.state(t, ctx) != settled || readForceCloseProviderProjection(t, ctx, f) != provider ||
		contractPayoutTestAccountAmount(t, ctx, networkId) != payerAccount {
		t.Fatal("payer debit or empty replay changed settlement identity or provider money")
	}
}

// The same accounting assertions also run with no failure or child context.
// This control must pass on baseline and candidate, proving the worker/replay
// policy independently of the operational-error classifier under test.
func TestForceCloseHealthyCheckpointWorkerCustodyAndReplay(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				ctx := WithProviderWorkSessionSource(t.Context(), nil)
				f := newForceCloseOperationalFixture(t, ctx, legacy)
				requireForceCloseOperationalRecovery(t, ctx, f, legacy, nil)
			})
		})
	}
}

func TestForceCloseCheckpointResourceFailurePreservesRecovery(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				ctx := WithProviderWorkSessionSource(t.Context(), nil)
				f := newForceCloseOperationalFixture(t, ctx, legacy)
				before := f.state(t, ctx)
				restore := forceCloseCheckpointFailure(ctx, "53200")
				defer restore()
				var proof []byte
				for pass := int64(1); pass <= 2; pass++ {
					count, err := ForceCloseOpenContractIds(ctx, f.cutoff, 10, 1, 0, 0)
					var pgError *pgconn.PgError
					if count != 1 || !errors.As(err, &pgError) || pgError.Code != "53200" {
						t.Fatal("real checkpoint UPDATE resource failure was not retained", count, err)
					}
					var attempts int64
					var called bool
					server.Db(ctx, func(conn server.PgConn) {
						server.Raise(conn.QueryRow(ctx, `SELECT last_value,is_called FROM synthetic_checkpoint_failure_attempts`).Scan(&attempts, &called))
					})
					if !called || attempts != pass {
						t.Fatal("failure did not witness exactly one original checkpoint UPDATE per pass", attempts)
					}
					retained := requireForceCloseOperationalPreserved(t, ctx, f, before, err)
					if pass == 2 && !bytes.Equal(proof, retained) {
						t.Fatal("failed replay replaced the retained proof")
					}
					proof = retained
				}
				restore()
				requireForceCloseOperationalRecovery(t, ctx, f, legacy, proof)
			})
		})
	}
}

func TestForceCloseCheckpointWaitDeadlinePreservesRecovery(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				ctx, cancel := context.WithTimeout(WithProviderWorkSessionSource(t.Context(), nil), 30*time.Second)
				defer cancel()
				f := newForceCloseOperationalFixture(t, ctx, legacy)
				before := f.state(t, ctx)
				conn := acquireContractLifecycleTestConnection(t, ctx)
				defer conn.Release()
				held, err := conn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer held.Rollback(ctx)
				var checkpoint bool
				server.Raise(held.QueryRow(ctx, `SELECT checkpoint FROM contract_close WHERE contract_id=$1 AND party=$2 FOR UPDATE`,
					f.contractId, ContractPartySource).Scan(&checkpoint))
				if !checkpoint {
					t.Fatal("deadline fixture lacks the original checkpoint")
				}
				blocker := contractLifecycleTestBackendPid(t, ctx, held)
				type childOwner struct {
					ctx    context.Context
					cancel context.CancelFunc
				}
				started := make(chan childOwner, 1)
				callCtx := context.WithValue(ctx, forceCloseContinuationContextKey{}, func(parent context.Context, id server.Id) context.Context {
					if id != f.contractId {
						return parent
					}
					child, stop := context.WithTimeout(parent, 3*time.Second)
					started <- childOwner{child, stop}
					return child
				})
				type result struct {
					count int64
					err   error
				}
				done := make(chan result, 1)
				go func() {
					var r result
					r.err = captureContractExpiryRepair(func() error {
						var err error
						r.count, err = ForceCloseOpenContractIds(callCtx, f.cutoff, 10, 1, 0, 0)
						return err
					})
					done <- r
				}()
				var child childOwner
				select {
				case child = <-started:
					defer child.cancel()
				case <-ctx.Done():
					t.Fatal("expiry did not commit proof and start its continuation", ctx.Err())
				}
				// The proof transaction already committed. Only the subsequent
				// checkpoint UPDATE needs this row lock; observe that real wait
				// before accepting any deadline result. The parent remains live.
				waitCtx, stopWait := context.WithCancel(child.ctx)
				defer stopWait()
				requireContractLifecycleBlockedBy(t, waitCtx, held, blocker)
				var r result
				select {
				case r = <-done:
				case <-ctx.Done():
					t.Fatal("expired continuation did not return under its live parent", ctx.Err())
				}
				if ctx.Err() != nil || child.ctx.Err() != context.DeadlineExceeded ||
					r.count != 1 || !errors.Is(r.err, context.DeadlineExceeded) {
					t.Fatal("witnessed child deadline was lost or replaced by parent cancellation", r.count, r.err)
				}
				server.Raise(held.Rollback(ctx))
				proof := requireForceCloseOperationalPreserved(t, ctx, f, before, r.err)
				requireForceCloseOperationalRecovery(t, ctx, f, legacy, proof)
			})
		})
	}
}

// Keep the existing opaque application exception fallback alongside the
// unchanged native insufficient-escrow controls requested by the gate.
func TestForceCloseCheckpointApplicationExceptionRetainsQuarantine(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newForceCloseOperationalFixture(t, ctx, false)
		before := f.state(t, ctx)
		restore := forceCloseCheckpointFailure(ctx, "P0001")
		defer restore()
		count, err := ForceCloseOpenContractIds(ctx, f.cutoff, 10, 1, 0, 0)
		var pgError *pgconn.PgError
		if count != 1 || !errors.As(err, &pgError) || pgError.Code != "P0001" {
			t.Fatal("application exception lost its original error", count, err)
		}
		after := f.state(t, ctx)
		if after.outcome != ContractOutcomeSettled || after.open || after.dispute || after.streamFound ||
			after.escrowSettled || after.escrowPayoutByteCount != 0 || after.providerEarnedByteCount != 0 ||
			after.payerBalanceByteCount != before.payerBalanceByteCount || after.netEscrowByteCount != 0 ||
			after.requestTokenByteCount != 0 || after.sourceByteCount != before.sourceByteCount ||
			after.destinationByteCount != before.destinationByteCount || !after.sourceCheckpoint || !after.destinationCheckpoint {
			t.Fatal("existing no-payout application quarantine changed", after)
		}
		if count, err := ForceCloseOpenContractIds(ctx, f.cutoff, 10, 1, 0, 0); err != nil || count != 0 || f.state(t, ctx) != after {
			t.Fatal("terminal quarantine replay changed accounting", count, err)
		}
	})
}
