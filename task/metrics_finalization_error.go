// Handback diagnostics describe observed attempt failures, never financial
// rollbacks or committed lifecycle events.
package task

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

var taskFinalizationErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "finalization_errors_total",
	Help:      "Failed finalization invocations per participating task by finite registered target and typed cause. Includes unacknowledged commits; does not prove rollback, financial failure, or distinct tasks.",
}, []string{"task", "cause"})

// Count each distinct member once in this failed invocation. Shared handbacks
// count their members, not one transaction. A known rollback followed by singles
// makes new attempts; cohort delegation to the batch owner is observed only by
// that owner. Compatibility fallback before a transaction is not a failure.
// IDs only deduplicate this bounded call and never become metric labels.
func (self *TaskWorker) observeTaskFinalizationFailure(results []*taskExecutionResult, failure any, observations ...*taskFinalizationObservation) {
	if failure == nil {
		return
	}
	cause := "non_error_panic"
	if err, ok := failure.(error); ok {
		cause = taskExecutionErrorCause(err)
	}
	var observation *taskFinalizationObservation
	if len(observations) != 0 {
		observation = observations[0]
	}
	phase := observation.failurePhase()
	elapsed := float64(0)
	if observation != nil {
		elapsed = max(0, time.Since(observation.started).Seconds())
	}
	seen := make(map[server.Id]bool, min(len(results), taskCompletionBatchLimit))
	for _, result := range results[:min(len(results), taskCompletionBatchLimit)] {
		if result == nil || result.task == nil || seen[result.task.TaskId] {
			continue
		}
		seen[result.task.TaskId] = true
		name := self.metricName(result.task.FunctionName)
		taskFinalizationErrorsTotal.WithLabelValues(name, cause).Inc()
		taskFinalizationPhaseErrorsTotal.WithLabelValues(name, phase, cause).Inc()
		if phase == "admission" {
			taskFinalizationAdmissionErrorsTotal.WithLabelValues(name, observation.admissionStage(), cause).Inc()
		}
	}
	if observation != nil && len(seen) != 0 {
		taskFinalizationFailureSeconds.WithLabelValues(phase).Observe(elapsed)
	}
}
