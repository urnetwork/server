// The notes of a live announcement's retried failures.
//
// A live announcement keeps its transport through a failed sync read or
// worker write and retries it. Each failure is counted, in
// urnetwork_connect_connection_sync_read_failures_total or
// urnetwork_connect_connection_worker_failures_total by operation, and logged
// at V(1).
//
// Safe for concurrent use.
package connect

import (
	"github.com/urnetwork/glog"
)

// The operations a live announcement retries.
const (
	liveFailureOperationSyncRead          = "sync_read"
	liveFailureOperationMeasurementWrite  = connectionWorkerOperationMeasurementWrite
	liveFailureOperationVerifyEgressLease = connectionWorkerOperationVerifyEgressLease
)

type liveFailureLog struct {
	logf func(format string, args ...any)
}

func newLiveFailureLog(logf func(format string, args ...any)) *liveFailureLog {
	return &liveFailureLog{
		logf: logf,
	}
}

var defaultLiveFailureLog = newLiveFailureLog(glog.Infof)

// Counts one failure. consecutiveCount is the operation's consecutive
// failures on its connection, this one included.
func (self *liveFailureLog) record(operation string, consecutiveCount int, failureErr error) {
	if operation == liveFailureOperationSyncRead {
		connectionSyncReadFailuresCounter.Inc()
	} else {
		connectionWorkerFailuresCounter.WithLabelValues(operation).Inc()
	}
	if glog.V(1) {
		self.logf("[t]%s failed, retrying (%d consecutive): %s\n", operation, consecutiveCount, failureErr)
	}
}
