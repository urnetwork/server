// Task lifecycle counters publish only acknowledged transaction outcomes.
package task

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

// New pending owners, completed owners and coalesced RunOnce attempts are
// separate events. An active-owner conflict may later insert one successor.
// Rollback/retry attempts publish nothing; unknown commit replies remain unknown.
var taskSubmittedCounter server.TxCommitCounter
var taskFinishedCounter server.TxCommitCounter
var taskBalkedCounter server.TxCommitCounter

var taskSubmittedMetric = prometheus.NewCounterFunc(prometheus.CounterOpts{
	Name: "urnetwork_task_submitted_total",
	Help: "New pending task rows observed after acknowledged commit, including continuations and RunOnce successors. Excludes coalesced conflicts and rolled-back attempts.",
}, func() float64 { return float64(taskSubmittedCounter.ConfirmedCount()) })

var taskFinishedMetric = prometheus.NewCounterFunc(prometheus.CounterOpts{
	Name: "urnetwork_task_finished_total",
	Help: "Pending tasks moved to finished_task after acknowledged commit. Includes durable post retries; does not imply optional post effects have completed.",
}, func() float64 { return float64(taskFinishedCounter.ConfirmedCount()) })

var taskBalkedMetric = prometheus.NewCounterFunc(prometheus.CounterOpts{
	Name: "urnetwork_task_balked_total",
	Help: "RunOnce submission conflicts observed after acknowledged commit, including accepted coalescing and IfAbsent refusals. An active conflict can still request a successor; rollback and admission refusals are excluded.",
}, func() float64 { return float64(taskBalkedCounter.ConfirmedCount()) })

// Register a fixed three-series surface with no task, account or key labels.
func init() {
	prometheus.MustRegister(taskSubmittedMetric, taskFinishedMetric, taskBalkedMetric)
}

// The insert result distinguishes a new owner from its RunOnce conflict without
// another query. Both events belong to this exact outer transaction attempt.
func observeTaskSubmissionInTx(tx server.PgTx, inserted bool) {
	if inserted {
		server.AddTxCommitCount(tx, &taskSubmittedCounter, 1)
	} else {
		server.AddTxCommitCount(tx, &taskBalkedCounter, 1)
	}
}
