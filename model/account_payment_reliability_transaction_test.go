// Reliability refresh, policy checks and allocation share the plan's rollback owner.
package model

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urnetwork/server"
)

// Record real driver activity from lock acquisition through the owning commit.
// Only synthetic window bounds and aggregate counts are retained.
type paymentReliabilityTransactionObserver struct {
	stateLock           sync.Mutex
	ownerPid            uint32
	active              bool
	statements          int
	foreignStatements   int
	nestedAcquires      int
	schemaChecks        int
	boundaryChecks      int
	aggregations        int
	aggregationRows     int64
	minBlock, maxBlock  int64
	aggregationDuration time.Duration
	queryErr            error
}

// The existing advisory-lock statement marks the real plan boundary in both arms.
func (self *paymentReliabilityTransactionObserver) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sql := strings.TrimSpace(data.SQL)
	if strings.Contains(sql, "pg_advisory_xact_lock") && len(data.Args) == 1 && data.Args[0] == paymentPlanLockKey {
		self.ownerPid = conn.PgConn().PID()
		self.active = true
	}
	if !self.active {
		return ctx
	}
	self.statements++
	if conn.PgConn().PID() != self.ownerPid {
		self.foreignStatements++
	}
	if strings.Contains(sql, "migration_catalog") || strings.Contains(sql, "pg_trigger") || strings.Contains(sql, "pg_attribute") || strings.Contains(sql, "to_regclass") {
		self.schemaChecks++
	}
	if strings.HasPrefix(sql, "SELECT earning_identity, identity_sha256, initial_config_sha256, prepared_at FROM provider_payout_boundary") {
		self.boundaryChecks++
	}
	if strings.HasPrefix(sql, "INSERT INTO network_connection_reliability_score") {
		self.aggregations++
		self.minBlock, _ = data.Args[0].(int64)
		self.maxBlock, _ = data.Args[1].(int64)
		return context.WithValue(ctx, self, time.Now())
	}
	if (sql == "commit" || sql == "rollback") && conn.PgConn().PID() == self.ownerPid {
		self.active = false
	}
	return ctx
}

// Completion accounts for the actual window aggregation, including failures.
func (self *paymentReliabilityTransactionObserver) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	started, matched := ctx.Value(self).(time.Time)
	if !matched {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.aggregationRows += data.CommandTag.RowsAffected()
	self.aggregationDuration += time.Since(started)
	self.queryErr = errors.Join(self.queryErr, data.Err)
}

// Acquiring even an idle second connection while planning is a second owner.
func (self *paymentReliabilityTransactionObserver) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.active {
		self.nestedAcquires++
	}
	return ctx
}

// Query completion supplies all observations needed after an acquisition.
func (self *paymentReliabilityTransactionObserver) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
}

// A fixed historical window has 8192 contributing rows and 8192 newer decoys.
// The preexisting score makes an independently committed refresh observable.
func paymentReliabilityTransactionFixture(t testing.TB, ctx context.Context) (*payoutTransitionCohort, time.Time, time.Time) {
	t.Helper()
	usePayoutTransition(t)
	f := newPayoutTransitionCohort(t, ctx)
	start := payoutTestCutoff.Add(-4 * time.Hour)
	end := start.Add(127 * ReliabilityBlockDuration)
	f.insert(t, ctx, start, &end, payoutTestCutoff.Add(time.Hour), 1024, UsdToNanoCents(1))
	countryId := server.NewId()
	clientIds := make([]server.Id, 64)
	for index := range clientIds {
		clientIds[index] = server.NewId()
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability
			(client_id, network_id, country_location_id, update_block_number, client_address_hash_count, location_count)
			SELECT client_id, $1, $2, $3, 1, 1 FROM unnest($4::uuid[]) AS ids(client_id)`,
			f.network, countryId, reliabilityBlockNumber(end), clientIds))
		// The drain stores validity explicitly; counters no longer generate it.
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_reliability
			(block_number, client_address_hash, network_id, client_id,
			connection_established_count, provide_enabled_count, receive_message_count, valid)
			SELECT block_number, decode('01', 'hex'), $1, client_id, 1, 1, 1, true
			FROM unnest($2::uuid[]) AS ids(client_id)
			CROSS JOIN generate_series($3::bigint, $3::bigint+255) AS blocks(block_number)`,
			f.network, clientIds, reliabilityBlockNumber(start)))
		var rows, contributing, newer int
		server.Raise(tx.QueryRow(ctx, `SELECT COUNT(*),
			COUNT(*) FILTER (WHERE reliability.valid AND location.valid AND $2 <= block_number AND block_number < $3),
			COUNT(*) FILTER (WHERE reliability.valid AND location.valid AND $3 <= block_number)
			FROM client_reliability AS reliability
			JOIN network_client_location_reliability AS location USING (client_id)
			WHERE reliability.network_id=$1`, f.network, reliabilityBlockNumber(start), reliabilityBlockNumber(end)+1).
			Scan(&rows, &contributing, &newer))
		if rows != 16384 || contributing != 8192 || newer != 8192 {
			t.Fatalf("reliability fixture rows/contributing/newer = %d/%d/%d, want 16384/8192/8192", rows, contributing, newer)
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_connection_reliability_score
			(network_id, country_location_id, independent_reliability_score, independent_reliability_weight,
			reliability_score, reliability_weight, min_block_number, max_block_number)
			VALUES($1,$2,7,7,7,7,0,1)`, f.network, countryId))
	})
	return f, start, end
}

// Fail after allocation to prove the refresh cannot escape its caller's rollback.
func TestPaymentPlanReliabilityRollsBackWithAllocation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, _, _ := paymentReliabilityTransactionFixture(t, ctx)
		rollbackErr := errors.New("synthetic payout allocation rollback")
		var attempted *PaymentPlan
		panicValue := server.HandleError(func() {
			_, planErr := createPaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0, false, func(tx server.PgTx, plan *PaymentPlan) {
				attempted = plan
				var score float64
				server.Raise(tx.QueryRow(ctx, `SELECT independent_reliability_score FROM network_connection_reliability_score WHERE network_id=$1`, f.network).Scan(&score))
				if score != 8192 || plan.SubsidyPayment == nil || len(plan.NetworkPayments) != 1 {
					t.Fatalf("rollback must follow the actual window refresh and allocation: score=%g plan=%+v", score, plan)
				}
				panic(rollbackErr)
			})
			server.Raise(planErr)
		})
		err, ok := panicValue.(error)
		if !ok || !errors.Is(err, rollbackErr) || attempted == nil {
			t.Fatal("fixture did not reach the allocation rollback", panicValue)
		}
		requirePaymentReliabilityRollback(t, ctx, f.network, attempted.PaymentPlanId)
	})
}

// The public dry-run mode computes the same window without retaining its score.
func TestPaymentPlanReliabilityDryRunRollsBack(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, start, end := paymentReliabilityTransactionFixture(t, ctx)
		plan, err := createPaymentPlan(ctx, payoutTransitionRevenueConfig(), true, 0, false, nil)
		if err != nil || plan == nil || plan.SubsidyPayment == nil || !plan.SubsidyPayment.StartTime.Equal(start) || !plan.SubsidyPayment.EndTime.Equal(end) {
			t.Fatal("dry run lost its selected subsidy window", plan, err)
		}
		requirePaymentReliabilityRollback(t, ctx, f.network, plan.PaymentPlanId)
	})
}

// Inspect after the transaction ends: no later recompute may hide an escaped write.
func requirePaymentReliabilityRollback(t testing.TB, ctx context.Context, networkId, planId server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var score float64
		var minBlock, maxBlock int64
		var payments, subsidies, points, paidSweeps int
		server.Raise(conn.QueryRow(ctx, `SELECT independent_reliability_score, min_block_number, max_block_number,
			(SELECT COUNT(*) FROM account_payment WHERE payment_plan_id=$2),
			(SELECT COUNT(*) FROM subsidy_payment WHERE payment_plan_id=$2),
			(SELECT COUNT(*) FROM account_point WHERE payment_plan_id=$2),
			(SELECT COUNT(*) FROM transfer_escrow_sweep WHERE network_id=$1 AND payment_id IS NOT NULL)
			FROM network_connection_reliability_score WHERE network_id=$1`, networkId, planId).
			Scan(&score, &minBlock, &maxBlock, &payments, &subsidies, &points, &paidSweeps))
		if score != 7 || minBlock != 0 || maxBlock != 1 || payments != 0 || subsidies != 0 || points != 0 || paidSweeps != 0 {
			t.Fatalf("refresh escaped allocation rollback: score=%g blocks=[%d,%d) payments=%d subsidies=%d points=%d paid_sweeps=%d",
				score, minBlock, maxBlock, payments, subsidies, points, paidSweeps)
		}
	})
}

// Exercise the real planner and retained earning identity on bounded input.
// Counts are the performance gate; elapsed aggregation time is evidence only.
func TestPaymentPlanReliabilityUsesOneBackendAndOneAggregation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, start, end := paymentReliabilityTransactionFixture(t, ctx)
		observer := &paymentReliabilityTransactionObserver{}
		scope, err := server.NewTestPgQueryScope(ctx, observer)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		plan, err := createPaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0, false, nil)
		if err != nil || plan == nil || plan.SubsidyPayment == nil || !plan.SubsidyPayment.StartTime.Equal(start) || !plan.SubsidyPayment.EndTime.Equal(end) {
			t.Fatal("committed plan lost its selected subsidy window", plan, err)
		}
		if observer.active || observer.ownerPid == 0 || observer.foreignStatements != 0 || observer.nestedAcquires != 0 {
			t.Errorf("plan acquired independent database owners: pid=%d active=%t foreign_statements=%d nested_acquires=%d",
				observer.ownerPid, observer.active, observer.foreignStatements, observer.nestedAcquires)
		}
		if observer.aggregations != 1 || observer.aggregationRows != 1 || observer.schemaChecks != 0 || observer.boundaryChecks != 1 || observer.queryErr != nil {
			t.Errorf("plan repeated or skipped its refresh/checks: aggregations=%d rows=%d schema=%d boundary=%d error=%v",
				observer.aggregations, observer.aggregationRows, observer.schemaChecks, observer.boundaryChecks, observer.queryErr)
		}
		if observer.minBlock != reliabilityBlockNumber(start) || observer.maxBlock != reliabilityBlockNumber(end)+1 || observer.statements > 64 {
			t.Errorf("plan expanded its aggregation window or round trips: blocks=[%d,%d) statements=%d", observer.minBlock, observer.maxBlock, observer.statements)
		}
		t.Logf("reliability input_rows=16384 contributing_rows=8192 aggregations=%d changed_rows=%d nested_acquires=%d foreign_statements=%d statements=%d aggregation_duration=%s",
			observer.aggregations, observer.aggregationRows, observer.nestedAcquires, observer.foreignStatements, observer.statements, observer.aggregationDuration)
		server.Db(ctx, func(conn server.PgConn) {
			var score float64
			server.Raise(conn.QueryRow(ctx, `SELECT independent_reliability_score FROM network_connection_reliability_score WHERE network_id=$1`, f.network).Scan(&score))
			if score != 8192 {
				t.Fatal("committed refresh included later unpaid history or lost contributing rows", score)
			}
		})
		restarted, err := createPaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0, false, nil)
		if err != nil || restarted == nil || len(restarted.NetworkPayments) != 0 || restarted.SubsidyPayment != nil || observer.aggregations != 1 {
			t.Fatal("planner restart repeated the allocated window or reliability aggregation", restarted, err, observer.aggregations)
		}
	})
}
