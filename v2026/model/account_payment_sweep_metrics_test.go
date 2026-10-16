// Deterministic assignment observations use the real planner boundary and one
// command owner. Every other transaction method is unavailable in this fixture.
package model

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/urnetwork/server/v2026"
)

type paymentSweepMetricTx struct {
	server.PgTx
	t     testing.TB
	exec  func() (server.PgTag, error)
	calls int
}

// Any added read/transaction method hits the absent embedded transaction. The
// sole command must retain the literal statement, zero bind values and caller.
func (self *paymentSweepMetricTx) Exec(ctx context.Context, sql string, args ...any) (server.PgTag, error) {
	self.t.Helper()
	self.calls++
	if self.calls != 1 || ctx != self.t.Context() || len(args) != 0 ||
		fmt.Sprintf("%x", sha256.Sum256([]byte(sql))) != "bfa63876efb5bab2340383dbc2b5f4f597a5b3b44a3caa9dca8c46e33837c5c5" {
		self.t.Fatal("assignment changed its SQL, owner, command count or arguments")
	}
	return self.exec()
}

// A fresh registry and explicit clock keep metrics deterministic and isolated.
func paymentSweepMetricsFixture(t testing.TB) (*PaymentPlanner, *paymentSweepMetricTx, *paymentSweepAssignmentMetricSet, *time.Time) {
	t.Helper()
	metrics := newPaymentSweepAssignmentMetricSet(prometheus.NewRegistry())
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	metrics.now = func() time.Time { return now }
	tx := &paymentSweepMetricTx{t: t}
	first, second, withheld, bonusOnly := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	planner := &PaymentPlanner{
		ctx:                t.Context(),
		tx:                 tx,
		networkPayments:    map[server.Id]*AccountPayment{first: {}, second: {}, bonusOnly: {}},
		networkSweepCounts: map[server.Id]int64{first: 3, second: 5, withheld: 100000000},
	}
	return planner, tx, metrics, &now
}

// Histogram inspection uses its actual collector, not a duplicate accumulator.
func paymentSweepMetricHistogram(t testing.TB, metric prometheus.Metric) *dto.Histogram {
	t.Helper()
	value := &dto.Metric{}
	if err := metric.Write(value); err != nil {
		t.Fatal(err)
	}
	if value.Histogram == nil {
		t.Fatal("expected an assignment histogram")
	}
	return value.Histogram
}

// Completion describes a statement result, never a committed payout.
func assertPaymentSweepMetricExit(t testing.TB, tx *paymentSweepMetricTx, metrics *paymentSweepAssignmentMetricSet, outcome string, duration float64) {
	t.Helper()
	if tx.calls != 1 || testutil.ToFloat64(metrics.inflight) != 0 || testutil.ToFloat64(metrics.inflightRows) != 0 {
		t.Fatal("assignment left an active observation or changed command count")
	}
	for _, candidate := range []string{"matched", "selection_changed", "error"} {
		metric := metrics.seconds.WithLabelValues(candidate).(prometheus.Metric)
		observed := paymentSweepMetricHistogram(t, metric)
		var count uint64
		var seconds float64
		if candidate == outcome {
			count, seconds = 1, duration
		}
		if observed.GetSampleCount() != count || observed.GetSampleSum() != seconds {
			t.Fatal("assignment exit lost its exact outcome or clock", candidate, observed)
		}
	}
}

func TestPaymentSweepAssignmentMetricsFiniteSchema(t *testing.T) {
	registry := prometheus.NewRegistry()
	newPaymentSweepAssignmentMetricSet(registry)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		"urnetwork_payment_sweep_assignment_selected_rows": 1,
		"urnetwork_payment_sweep_assignment_inflight":      1,
		"urnetwork_payment_sweep_assignment_inflight_rows": 1,
		"urnetwork_payment_sweep_assignment_seconds":       3,
	}
	if len(families) != len(want) {
		t.Fatal("assignment metric family set changed", len(families))
	}
	for _, family := range families {
		if len(family.Metric) != want[family.GetName()] {
			t.Fatal("assignment metric cardinality changed", family.GetName())
		}
		for _, metric := range family.Metric {
			if family.GetName() != "urnetwork_payment_sweep_assignment_seconds" {
				if len(metric.Label) != 0 {
					t.Fatal("assignment size retained a caller identifier")
				}
			} else if len(metric.Label) != 1 || metric.Label[0].GetName() != "outcome" ||
				(metric.Label[0].GetValue() != "matched" && metric.Label[0].GetValue() != "selection_changed" && metric.Label[0].GetValue() != "error") {
				t.Fatal("assignment outcome retained an unbounded label")
			}
		}
	}
}

func TestPaymentSweepAssignmentMetricsSelectedRowsBeforeExec(t *testing.T) {
	planner, tx, metrics, now := paymentSweepMetricsFixture(t)
	tx.exec = func() (server.PgTag, error) {
		selected := paymentSweepMetricHistogram(t, metrics.selectedRows)
		if selected.GetSampleCount() != 1 || selected.GetSampleSum() != 8 ||
			testutil.ToFloat64(metrics.inflight) != 1 || testutil.ToFloat64(metrics.inflightRows) != 8 {
			t.Fatal("live assignment lost its selected count or included withheld rows", selected)
		}
		*now = now.Add(2*time.Hour + time.Second)
		return pgconn.NewCommandTag("UPDATE 8"), nil
	}
	planner.assignSweeps(metrics)
	assertPaymentSweepMetricExit(t, tx, metrics, "matched", 7201)
}

func TestPaymentSweepAssignmentMetricsPreserveOwnerRefusal(t *testing.T) {
	planner, tx, metrics, now := paymentSweepMetricsFixture(t)
	tx.exec = func() (server.PgTag, error) {
		*now = now.Add(3 * time.Second)
		return pgconn.NewCommandTag("UPDATE 7"), nil
	}
	failure := server.HandleError(func() { planner.assignSweeps(metrics) })
	if failure != errPaymentPlanSelectionChanged {
		t.Fatal("assignment no longer refuses the exact owner-count mismatch", failure)
	}
	assertPaymentSweepMetricExit(t, tx, metrics, "selection_changed", 3)
}

func TestPaymentSweepAssignmentMetricsPreserveCancellation(t *testing.T) {
	planner, tx, metrics, now := paymentSweepMetricsFixture(t)
	tx.exec = func() (server.PgTag, error) {
		*now = now.Add(5 * time.Second)
		return pgconn.CommandTag{}, context.Canceled
	}
	failure := server.HandleError(func() { planner.assignSweeps(metrics) })
	if failure != context.Canceled {
		t.Fatal("assignment observation replaced the driver's cancellation", failure)
	}
	assertPaymentSweepMetricExit(t, tx, metrics, "error", 5)
}

func TestPaymentSweepAssignmentMetricsPreservePanic(t *testing.T) {
	planner, tx, metrics, now := paymentSweepMetricsFixture(t)
	sentinel := &struct{ value int }{value: 17}
	tx.exec = func() (server.PgTag, error) {
		*now = now.Add(7 * time.Second)
		panic(sentinel)
	}
	var failure any
	func() {
		defer func() { failure = recover() }()
		planner.assignSweeps(metrics)
	}()
	if failure != sentinel {
		t.Fatal("assignment observation swallowed or replaced a panic")
	}
	assertPaymentSweepMetricExit(t, tx, metrics, "error", 7)
}

func TestPaymentSweepAssignmentMetricsZeroRowsStillExecutes(t *testing.T) {
	planner, tx, metrics, _ := paymentSweepMetricsFixture(t)
	planner.networkPayments = map[server.Id]*AccountPayment{}
	tx.exec = func() (server.PgTag, error) {
		selected := paymentSweepMetricHistogram(t, metrics.selectedRows)
		if selected.GetSampleCount() != 1 || selected.GetSampleSum() != 0 ||
			testutil.ToFloat64(metrics.inflight) != 1 || testutil.ToFloat64(metrics.inflightRows) != 0 {
			t.Fatal("zero-row assignment changed its command boundary", selected)
		}
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	planner.assignSweeps(metrics)
	assertPaymentSweepMetricExit(t, tx, metrics, "matched", 0)
}
