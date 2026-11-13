// A failed handback's source phase is separate from its typed error cause.
package task

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

var taskFinalizationPhaseErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "finalization_phase_errors_total",
	Help:      "Failed finalization invocations per participating task by finite registered target, entered source phase and typed cause. Acquisition does not distinguish Ping from pool waiting; commit errors do not prove rollback.",
}, []string{"task", "phase", "cause"})

var taskFinalizationAdmissionErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "finalization_admission_errors_total",
	Help:      "Failed finalization admission attempts per participating task by finite entered stage and typed cause. Acknowledged busy wait does not prove one continuously held key; probe includes Query and reply validation. Existing session quarantine is preparation and excluded.",
}, []string{"task", "stage", "cause"})

var taskFinalizationFailureSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "finalization_failure_seconds",
	Help:      "Failed finalization invocation wall time including unwinding and cleanup, once per invocation by source phase. This is not phase-only time, SQL runtime or financial rollback latency.",
	Buckets:   []float64{.001, .01, .05, .1, .5, 1, 2, 5, 10, 30, 60, 120},
}, []string{"phase"})

type taskFinalizationBodyPhase uint8

const (
	taskFinalizationTransaction taskFinalizationBodyPhase = iota
	taskFinalizationQueueUpdate
	taskFinalizationTransactionalPost
)

// One collector owns this observation; it is read only after the finalizer has
// unwound. It contains no task identities, SQL, arguments or arbitrary strings.
type taskFinalizationObservation struct {
	db           server.DbPhaseObservation
	started      time.Time
	body         taskFinalizationBodyPhase
	acknowledged bool
}

func newTaskFinalizationObservation() *taskFinalizationObservation {
	return &taskFinalizationObservation{started: time.Now()}
}

func (self *taskFinalizationObservation) failurePhase() string {
	if self == nil {
		return "unknown"
	}
	if self.acknowledged {
		return "acknowledged"
	}
	switch self.db.Phase() {
	case server.DbOperationUnknown, server.DbOperationOwnershipConfiguration:
		return "preparation"
	case server.DbOperationAcquire:
		return "acquire"
	case server.DbOperationOwnershipAcquire:
		return "ownership_acquire"
	case server.DbOperationAdmission:
		return "admission"
	case server.DbOperationSessionSetup:
		return "session_setup"
	case server.DbOperationBegin:
		return "begin"
	case server.DbOperationCallback:
		if self.body == taskFinalizationTransactionalPost {
			return "transactional_post"
		}
		if self.body == taskFinalizationQueueUpdate {
			return "queue_update"
		}
		return "transaction"
	case server.DbOperationCommit:
		return "commit"
	case server.DbOperationAcknowledged:
		return "acknowledged"
	case server.DbOperationPostCommit:
		return "post_commit"
	default:
		return "unknown"
	}
}

func (self *taskFinalizationObservation) admissionStage() string {
	if self == nil {
		return "unknown"
	}
	switch self.db.AdmissionStage() {
	case server.DbAdmissionPrecheck:
		return "precheck"
	case server.DbAdmissionProbe:
		return "probe"
	case server.DbAdmissionCleanup:
		return "cleanup"
	case server.DbAdmissionAcknowledgedBusyWait:
		return "acknowledged_busy_wait"
	default:
		return "unknown"
	}
}
