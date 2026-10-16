// Actual planner transactions must retain one owner of each legacy obligation.
// PostgreSQL insert/lock barriers force the interleavings without timing guesses.
package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

type paymentPlanOwnershipOutcome struct {
	plan       *PaymentPlan
	err        error
	panicValue any
}

// The buffered result remains owned until cleanup joins every started planner.
func startPaymentPlanOwnershipAttempt(ctx context.Context, config *SubsidyConfig, dryRun bool, duration time.Duration, joined *sync.WaitGroup, results chan<- paymentPlanOwnershipOutcome) {
	joined.Go(func() {
		value := paymentPlanOwnershipOutcome{}
		value.panicValue = server.HandleError(func() {
			value.plan, value.err = CreatePaymentPlan(ctx, config, dryRun, duration)
		})
		results <- value
	})
}

// A direct connection owns only the synthetic barrier, never the planner's
// writes. Releasing its transaction permits the real application to commit.
func holdPaymentPlanOwnershipBarrier(t testing.TB, ctx context.Context, key string) (server.PgConn, server.PgTx, int32) {
	t.Helper()
	conn, err := server.AcquireMaintenanceDbConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		t.Fatal(err)
	}
	var pid int32
	server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key))
	server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	return conn, tx, pid
}

// The trigger runs after the actual selection, subsidy and point allocation,
// immediately before the public planner publishes its account_payment row.
func installPaymentPlanOwnershipInsertBarrier(t testing.TB, ctx context.Context, networkId server.Id) string {
	t.Helper()
	key := "synthetic-payment-plan-insert/" + networkId.String()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
CREATE FUNCTION synthetic_payment_plan_insert_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(TG_ARGV[0], 0));
    RETURN NEW;
END $$;
CREATE TRIGGER synthetic_payment_plan_insert_barrier BEFORE INSERT ON account_payment
    FOR EACH ROW WHEN (NEW.network_id = '%s'::uuid)
    EXECUTE FUNCTION synthetic_payment_plan_insert_barrier('%s');`, networkId.String(), key)))
	})
	return key
}

// Count the real lock dependency chain. With the fix, the second planner waits
// behind the first planner; without it both reach the synthetic insert barrier.
func waitPaymentPlanOwnershipBlocked(t testing.TB, ctx context.Context, tx server.PgTx, holderPid int32, expected int, results <-chan paymentPlanOwnershipOutcome) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked int
		server.Raise(tx.QueryRow(ctx, `WITH RECURSIVE waiters(pid) AS (
			SELECT DISTINCT pid FROM pg_locks WHERE locktype='advisory' AND NOT granted
			AND $1::int=ANY(pg_blocking_pids(pid))
			UNION
			SELECT DISTINCT child.pid FROM pg_locks child JOIN waiters parent
			ON parent.pid=ANY(pg_blocking_pids(child.pid))
			WHERE child.locktype='advisory' AND NOT child.granted
		) SELECT COUNT(*) FROM waiters`, holderPid).Scan(&blocked))
		if blocked == expected {
			return
		}
		select {
		case result := <-results:
			t.Fatalf("planner escaped the original-allocation barrier: plan=%+v error=%v panic=%v", result.plan, result.err, result.panicValue)
		case <-ctx.Done():
			t.Fatal("planner did not reach the database ownership barrier", blocked, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Cancellation may retain the database wrapper's historical panic contract,
// but its cause must remain observable and no plan may have been published.
func requirePaymentPlanOwnershipCanceled(t testing.TB, value paymentPlanOwnershipOutcome) {
	t.Helper()
	cause := value.err
	if value.panicValue != nil {
		panicErr, ok := value.panicValue.(error)
		if !ok {
			t.Fatal("planner cancellation became an untyped panic", value.panicValue)
		}
		cause = errors.Join(cause, panicErr)
	}
	if value.plan != nil || !errors.Is(cause, context.Canceled) {
		t.Fatal("canceled planner published a plan or discarded its cause", value.plan, cause)
	}
}

// One committed allocation must retain the exact pre-cutoff sweeps, while
// cutoff-time completed work remains unassigned to a USDC payment on restart.
func TestProviderPaymentConcurrentPlannersRetainOneOriginalAllocation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		usePayoutTransition(t)
		f := newPayoutTransitionCohort(t, ctx)
		closed := payoutTestCutoff.Add(-time.Microsecond)
		legacy := f.insert(t, ctx, closed.Add(-90*time.Second), &closed, payoutTestCutoff.Add(time.Hour), 17, UsdToNanoCents(1))
		current := f.insert(t, ctx, closed, &payoutTestCutoff, payoutTestCutoff.Add(time.Hour), 23, UsdToNanoCents(2))
		config := payoutTransitionRevenueConfig()
		config.MinPayoutUsd = 100
		barrier := installPaymentPlanOwnershipInsertBarrier(t, ctx, f.network)
		conn, held, holderPid := holdPaymentPlanOwnershipBarrier(t, ctx, barrier)
		defer conn.Release()
		var joined sync.WaitGroup
		defer func() {
			cancel()
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), server.PgRollbackTimeout)
			defer cleanupCancel()
			_ = held.Rollback(cleanupCtx)
			joined.Wait()
		}()
		results := make(chan paymentPlanOwnershipOutcome, 2)
		startPaymentPlanOwnershipAttempt(ctx, config, false, 0, &joined, results)
		startPaymentPlanOwnershipAttempt(ctx, config, false, 4*24*time.Hour, &joined, results)
		waitPaymentPlanOwnershipBlocked(t, ctx, held, holderPid, 2, results)
		server.Raise(held.Rollback(ctx))
		joined.Wait()
		var payment *AccountPayment
		for range 2 {
			value := <-results
			if value.err != nil || value.panicValue != nil || value.plan == nil {
				t.Fatal("concurrent planners did not observe the committed original", value.err, value.panicValue)
			}
			if candidate := value.plan.NetworkPayments[f.network]; candidate != nil {
				if payment != nil {
					t.Fatal("concurrent planners allocated the same original obligation twice", payment.PaymentId, candidate.PaymentId)
				}
				payment = candidate
			}
		}
		if payment == nil || payment.PayoutByteCount != 17 || payment.SubsidyPayout <= 0 || payment.Payout != UsdToNanoCents(1)+payment.SubsidyPayout {
			t.Fatal("original legacy amount or completed bytes changed", payment)
		}
		if err := RequireProviderUsdcPayment(ctx, payment.PaymentId); err != nil {
			t.Fatal("original pre-cutoff obligation cannot still settle", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var payments, windows int
			var retained, unassigned bool
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT COUNT(*) FROM account_payment WHERE network_id=$1),
				(SELECT COUNT(*) FROM subsidy_payment),
				(SELECT payment_id=$2 FROM transfer_escrow_sweep WHERE contract_id=$3),
				(SELECT payment_id IS NULL FROM transfer_escrow_sweep WHERE contract_id=$4)`,
				f.network, payment.PaymentId, legacy, current).Scan(&payments, &windows, &retained, &unassigned))
			if payments != 1 || windows != 1 || !retained || !unassigned {
				t.Fatal("concurrent planning duplicated or reassigned earning authority", payments, windows, retained, unassigned)
			}
		})
		restarted, err := CreatePaymentPlan(ctx, config, false, 0)
		if err != nil || restarted == nil || len(restarted.NetworkPayments) != 0 || restarted.SubsidyPayment != nil {
			t.Fatal("planner restart repeated the retained allocation", restarted, err)
		}
		usage, err := GetStEpochProviderUsage(ctx, payoutTestCutoff, payoutTestCutoff.Add(time.Hour))
		if err != nil || len(usage) != 1 || usage[0].PayoutByteCount != 23 {
			t.Fatal("legacy concurrency or restart consumed post-cutoff completed work", usage, err)
		}
	})
}

// Every public mode waits before selection. Canceled contenders cannot hold
// the next invocation's ownership or publish a dry-run allocation.
func TestProviderPaymentPlannerCanceledWaitersReleaseEveryMode(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		usePayoutTransition(t)
		f := newPayoutTransitionCohort(t, ctx)
		closed := payoutTestCutoff.Add(-time.Microsecond)
		legacy := f.insert(t, ctx, closed, &closed, payoutTestCutoff.Add(time.Hour), 17, UsdToNanoCents(1))
		config := payoutTransitionRevenueConfig()
		for _, mode := range []struct {
			dryRun   bool
			duration time.Duration
		}{{dryRun: false}, {dryRun: false, duration: 4 * 24 * time.Hour}, {dryRun: true}, {dryRun: true, duration: 4 * 24 * time.Hour}} {
			func() {
				conn, held, holderPid := holdPaymentPlanOwnershipBarrier(t, ctx, paymentPlanLockKey)
				defer conn.Release()
				attemptCtx, cancelAttempt := context.WithCancel(ctx)
				var joined sync.WaitGroup
				defer func() {
					cancelAttempt()
					cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), server.PgRollbackTimeout)
					defer cleanupCancel()
					_ = held.Rollback(cleanupCtx)
					joined.Wait()
				}()
				results := make(chan paymentPlanOwnershipOutcome, 1)
				startPaymentPlanOwnershipAttempt(attemptCtx, config, mode.dryRun, mode.duration, &joined, results)
				waitPaymentPlanOwnershipBlocked(t, ctx, held, holderPid, 1, results)
				cancelAttempt()
				joined.Wait()
				requirePaymentPlanOwnershipCanceled(t, <-results)
				server.Raise(held.Rollback(ctx))
			}()
		}
		preview, err := CreatePaymentPlan(ctx, config, true, 0)
		if err != nil || preview == nil || preview.NetworkPayments[f.network] == nil {
			t.Fatal("fresh dry run could not reacquire released planner ownership", preview, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var payments int
			var unassigned bool
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM account_payment),
				(SELECT payment_id IS NULL FROM transfer_escrow_sweep WHERE contract_id=$1)`, legacy).Scan(&payments, &unassigned))
			if payments != 0 || !unassigned {
				t.Fatal("canceled or dry-run owner published an allocation", payments, unassigned)
			}
		})
		restarted, err := CreatePaymentPlan(ctx, config, false, 0)
		if err != nil || restarted == nil || restarted.NetworkPayments[f.network] == nil || restarted.NetworkPayments[f.network].PayoutByteCount != 17 {
			t.Fatal("fresh planner lost the original unpaid work", restarted, err)
		}
	})
}

// Cancel the active owner after it staged subsidies/points and before its
// payment insert. The next process must recover the same original unpaid work.
func TestProviderPaymentPlannerOwnerCancellationRestartsOriginalWork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		usePayoutTransition(t)
		f := newPayoutTransitionCohort(t, ctx)
		closed := payoutTestCutoff.Add(-time.Microsecond)
		legacy := f.insert(t, ctx, closed.Add(-90*time.Second), &closed, payoutTestCutoff.Add(time.Hour), 17, UsdToNanoCents(1))
		config := payoutTransitionRevenueConfig()
		config.MinPayoutUsd = 100
		barrier := installPaymentPlanOwnershipInsertBarrier(t, ctx, f.network)
		conn, held, holderPid := holdPaymentPlanOwnershipBarrier(t, ctx, barrier)
		defer conn.Release()
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		var joined sync.WaitGroup
		defer func() {
			cancelAttempt()
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), server.PgRollbackTimeout)
			defer cleanupCancel()
			_ = held.Rollback(cleanupCtx)
			joined.Wait()
		}()
		results := make(chan paymentPlanOwnershipOutcome, 1)
		startPaymentPlanOwnershipAttempt(attemptCtx, config, false, 0, &joined, results)
		waitPaymentPlanOwnershipBlocked(t, ctx, held, holderPid, 1, results)
		cancelAttempt()
		joined.Wait()
		requirePaymentPlanOwnershipCanceled(t, <-results)
		server.Raise(held.Rollback(ctx))
		server.Db(ctx, func(conn server.PgConn) {
			var payments, windows, points int
			var unassigned bool
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM account_payment),
				(SELECT COUNT(*) FROM subsidy_payment),(SELECT COUNT(*) FROM account_point),
				(SELECT payment_id IS NULL FROM transfer_escrow_sweep WHERE contract_id=$1)`, legacy).Scan(&payments, &windows, &points, &unassigned))
			if payments != 0 || windows != 0 || points != 0 || !unassigned {
				t.Fatal("canceled owner committed a partial financial allocation", payments, windows, points, unassigned)
			}
		})
		restarted, err := CreatePaymentPlan(ctx, config, false, 0)
		if err != nil || restarted == nil || restarted.NetworkPayments[f.network] == nil || restarted.NetworkPayments[f.network].SubsidyPayout <= 0 {
			t.Fatal("restart did not retain the original subsidy-bearing work", restarted, err)
		}
		if err := RequireProviderUsdcPayment(ctx, restarted.NetworkPayments[f.network].PaymentId); err != nil {
			t.Fatal("restarted original obligation lost earning authority", err)
		}
	})
}

// A nonparticipating writer changes an already selected canceled owner. The
// stale plan must roll back its payment and points instead of taking that row.
func TestProviderPaymentPlannerFinalizationRefusesChangedOriginalOwner(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		usePayoutTransition(t)
		f := newPayoutTransitionCohort(t, ctx)
		closed := payoutTestCutoff.Add(-time.Microsecond)
		legacy := f.insert(t, ctx, closed, &closed, payoutTestCutoff.Add(time.Hour), 17, UsdToNanoCents(1))
		config := payoutTransitionRevenueConfig()
		config.ForcePoints = true
		original, err := CreatePaymentPlan(ctx, config, false, 0)
		if err != nil || original == nil || original.NetworkPayments[f.network] == nil {
			t.Fatal("original payment fixture missing", original, err)
		}
		payment := original.NetworkPayments[f.network]
		if err := CancelPayment(ctx, payment.PaymentId); err != nil {
			t.Fatal(err)
		}
		var originalPoints int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM account_point`).Scan(&originalPoints))
		})
		barrier := installPaymentPlanOwnershipInsertBarrier(t, ctx, f.network)
		conn, held, holderPid := holdPaymentPlanOwnershipBarrier(t, ctx, barrier)
		defer conn.Release()
		var joined sync.WaitGroup
		defer func() {
			cancel()
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), server.PgRollbackTimeout)
			defer cleanupCancel()
			_ = held.Rollback(cleanupCtx)
			joined.Wait()
		}()
		results := make(chan paymentPlanOwnershipOutcome, 1)
		startPaymentPlanOwnershipAttempt(ctx, config, false, 0, &joined, results)
		waitPaymentPlanOwnershipBlocked(t, ctx, held, holderPid, 1, results)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow_sweep SET payment_id=NULL WHERE contract_id=$1`, legacy))
		})
		server.Raise(held.Rollback(ctx))
		joined.Wait()
		value := <-results
		if value.panicValue != nil || value.plan != nil || !errors.Is(value.err, errPaymentPlanSelectionChanged) {
			t.Fatal("stale planner overwrote changed original ownership", value.plan, value.err, value.panicValue)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var payments, points int
			var retained, unassigned bool
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM account_payment),
				(SELECT COUNT(*) FROM account_point),(SELECT canceled AND NOT completed FROM account_payment WHERE payment_id=$1),
				(SELECT payment_id IS NULL FROM transfer_escrow_sweep WHERE contract_id=$2)`, payment.PaymentId, legacy).Scan(&payments, &points, &retained, &unassigned))
			if payments != 1 || points != originalPoints || !retained || !unassigned {
				t.Fatal("ownership refusal left partial payment/points or changed original debt", payments, points, retained, unassigned)
			}
		})
		restarted, err := CreatePaymentPlan(ctx, config, false, 0)
		if err != nil || restarted == nil || restarted.NetworkPayments[f.network] == nil || restarted.NetworkPayments[f.network].Payout != payment.Payout {
			t.Fatal("retry failed to read and allocate the new sweep owner exactly", restarted, err)
		}
	})
}
