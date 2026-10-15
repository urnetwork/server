// The notes of a live announcement's retried failures.
//
// A live announcement keeps its transport through a failed sync read or
// worker write and retries it. Each failure is counted, in
// urnetwork_connect_connection_sync_read_failures_total or
// urnetwork_connect_connection_worker_failures_total by operation, and logged
// at V(1). A failure that persists would leave nothing in the default log, so
// the failures are also logged unconditionally: the first at once, then at
// most one line per interval, with the failures by operation since the last
// line and the latest error. Failures after the last line are logged with the
// next failure, and are always in the counters. The limiter is the
// process's, like the log it writes, so the bound holds however many
// connections fail together.
//
// Safe for concurrent use.
package connect

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"
)

// The operations a live announcement retries.
const (
	liveFailureOperationSyncRead          = "sync_read"
	liveFailureOperationMeasurementWrite  = connectionWorkerOperationMeasurementWrite
	liveFailureOperationVerifyEgressLease = connectionWorkerOperationVerifyEgressLease
)

// between unconditional lines
const liveFailureLogInterval = time.Minute

type liveFailureLog struct {
	logf     func(format string, args ...any)
	interval time.Duration
	now      func() time.Time

	stateLock sync.Mutex
	// zero before the first line
	lastLineTime time.Time
	// failures since the last line, by operation
	operationCounts map[string]int
}

func newLiveFailureLog(logf func(format string, args ...any)) *liveFailureLog {
	return &liveFailureLog{
		logf:            logf,
		interval:        liveFailureLogInterval,
		now:             time.Now,
		operationCounts: map[string]int{},
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

	line := func() string {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.operationCounts[operation] += 1
		now := self.now()
		window := "first"
		if !self.lastLineTime.IsZero() {
			sinceLastLine := now.Sub(self.lastLineTime)
			if sinceLastLine < self.interval {
				return ""
			}
			window = fmt.Sprintf("in %s", sinceLastLine.Round(time.Second))
		}
		operations := []string{}
		for countedOperation := range self.operationCounts {
			operations = append(operations, countedOperation)
		}
		slices.Sort(operations)
		counts := []string{}
		for _, countedOperation := range operations {
			counts = append(counts, fmt.Sprintf("%s=%d", countedOperation, self.operationCounts[countedOperation]))
		}
		self.lastLineTime = now
		self.operationCounts = map[string]int{}
		return fmt.Sprintf(
			"[t]live connection failures retried with the transport kept (%s): %s; latest %s: %s",
			window,
			strings.Join(counts, " "),
			operation,
			failureErr,
		)
	}()
	if line != "" {
		self.logf("%s\n", line)
	}
}
