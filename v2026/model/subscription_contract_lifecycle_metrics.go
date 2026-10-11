// Counts first persisted opens and terminal closes at their commit owners.
package model

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

// Process-local counters count acknowledged commits, including zero-byte and
// no-escrow contracts. Reuse, rollback and terminal replay add no event. A close
// includes quarantine and deletion of an unresolved contract; deleting an
// existing terminal outcome cannot close it again. These are not crash-durable
// history and do not assert that Redis debit rows already applied.
var contractOpenedCounter server.TxCommitCounter
var contractClosedCounter server.TxCommitCounter

var contractOpenedMetric = prometheus.NewCounterFunc(prometheus.CounterOpts{
	Name: "urnetwork_contract_opened_total",
	Help: "First contract inserts observed after a successful transaction commit reply. Includes every reservation backend; excludes reuse and rolled-back attempts.",
}, func() float64 { return float64(contractOpenedCounter.ConfirmedCount()) })

var contractClosedMetric = prometheus.NewCounterFunc(prometheus.CounterOpts{
	Name: "urnetwork_contract_closed_total",
	Help: "First terminal contract transitions observed after a successful transaction commit reply, including malformed quarantine and deletion without a prior outcome. This is terminal closure, not completion of asynchronous financial projections.",
}, func() float64 { return float64(contractClosedCounter.ConfirmedCount()) })

func init() {
	prometheus.MustRegister(contractOpenedMetric, contractClosedMetric)
}
