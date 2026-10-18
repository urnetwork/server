// Observes the existing assignment statement without extra storage work.
// Counts include retries and dry runs, not committed payouts or money amounts.
package model

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
)

type paymentSweepAssignmentMetricSet struct {
	selectedRows prometheus.Histogram
	inflight     prometheus.Gauge
	inflightRows prometheus.Gauge
	seconds      *prometheus.HistogramVec
	now          func() time.Time
}

// Every outcome is fixed; no plan, network, contract, SQL or error becomes a label.
func newPaymentSweepAssignmentMetricSet(registerer prometheus.Registerer) *paymentSweepAssignmentMetricSet {
	metrics := &paymentSweepAssignmentMetricSet{
		selectedRows: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "urnetwork_payment_sweep_assignment_selected_rows",
			Help:    "Rows selected for each sweep assignment attempt, including retries and dry runs; not committed payouts.",
			Buckets: []float64{0, 1, 100, 1000, 10000, 100000, 1000000, 10000000, 100000000, 1000000000},
		}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "urnetwork_payment_sweep_assignment_inflight",
			Help: "Sweep assignment statements currently running in this process.",
		}),
		inflightRows: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "urnetwork_payment_sweep_assignment_inflight_rows",
			Help: "Selected rows across current sweep assignment attempts; not rows already updated or committed.",
		}),
		seconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "urnetwork_payment_sweep_assignment_seconds",
			Help:    "Sweep assignment statement residence through row-count validation; matched is not transaction commit, and error includes panic.",
			Buckets: []float64{.01, .1, 1, 10, 60, 300, 1800, 7200, 21600},
		}, []string{"outcome"}),
		now: time.Now,
	}
	for _, outcome := range []string{"matched", "selection_changed", "error"} {
		metrics.seconds.WithLabelValues(outcome)
	}
	registerer.MustRegister(metrics.selectedRows, metrics.inflight, metrics.inflightRows, metrics.seconds)
	return metrics
}

var paymentSweepAssignmentMetrics = newPaymentSweepAssignmentMetricSet(prometheus.DefaultRegisterer)

// The owning transaction, literal SQL and exact selected-owner refusal are
// unchanged. Observations finish on every unwind without swallowing its cause.
func (self *PaymentPlanner) assignSweeps(metrics *paymentSweepAssignmentMetricSet) {
	// Withheld networks are absent from networkPayments. Their selected rows
	// cannot contribute to this assignment's expected cardinality.
	var expectedSweepCount int64
	for networkId := range self.networkPayments {
		expectedSweepCount += self.networkSweepCounts[networkId]
	}
	start := metrics.now()
	metrics.selectedRows.Observe(float64(expectedSweepCount))
	metrics.inflight.Inc()
	metrics.inflightRows.Add(float64(expectedSweepCount))
	outcome := "error"
	defer func() {
		seconds := max(0, metrics.now().Sub(start).Seconds())
		metrics.inflight.Dec()
		metrics.inflightRows.Sub(float64(expectedSweepCount))
		metrics.seconds.WithLabelValues(outcome).Observe(seconds)
		glog.Infof("[plan]sweep assignment finished selected_rows=%d outcome=%s seconds=%.3f\n", expectedSweepCount, outcome, seconds)
	}()
	// The same finite fields remain observable for CLI planners without a scrape endpoint.
	glog.Infof("[plan]sweep assignment started selected_rows=%d\n", expectedSweepCount)

	// A nonparticipating writer cannot replace the selected original owner and
	// leave this plan's amounts/points committed against somebody else's sweeps.
	tag := server.RaisePgResult(self.tx.Exec(
		self.ctx,
		paymentPlanAssignSweepsSql,
	))
	if tag.RowsAffected() != expectedSweepCount {
		outcome = "selection_changed"
		panic(errPaymentPlanSelectionChanged)
	}
	outcome = "matched"
}
