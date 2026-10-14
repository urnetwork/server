// A live announcement retries a failed sync read or worker write with its
// transport kept. A failure that persists must reach the default log: before,
// each was logged only at V(1), so a log at the default verbosity showed
// nothing while the connections' reads and writes kept failing.
package connect

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/glog"
)

// Collects formatted log lines. Safe for concurrent use.
type logLinesForTest struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *logLinesForTest) logf(format string, args ...any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, fmt.Sprintf(format, args...))
}

func (self *logLinesForTest) take() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	lines := self.lines
	self.lines = nil
	return lines
}

func requireDefaultVerbosityForTest(t *testing.T) {
	t.Helper()
	if glog.V(1) {
		t.Skip("the test checks the default verbosity, and V(1) is on")
	}
}

// One failed sync read reaches the default log, with its operation and
// error.
func TestLiveFailureReachesTheDefaultLog(t *testing.T) {
	requireDefaultVerbosityForTest(t)
	logLines := &logLinesForTest{}
	failureLog := newLiveFailureLog(logLines.logf)

	failureLog.record(liveFailureOperationSyncRead, 1, errors.New("test: the database refused the read"))

	lines := logLines.take()
	if len(lines) == 0 {
		t.Fatal("a failed sync read of a live announcement left nothing in the default log; it was logged only at V(1)")
	}
	if !strings.Contains(lines[0], liveFailureOperationSyncRead) || !strings.Contains(lines[0], "test: the database refused the read") {
		t.Fatalf("the failure's line does not name its operation and error: %q", lines[0])
	}
}

// A database outage fails every connection's reads and writes together; the
// default log gets one line for them, not one per failure.
func TestLiveFailureLogBoundsAFlood(t *testing.T) {
	requireDefaultVerbosityForTest(t)
	logLines := &logLinesForTest{}
	failureLog := newLiveFailureLog(logLines.logf)

	failureErr := errors.New("test: the database refused the write")
	for i := 0; i < 10000; i += 1 {
		failureLog.record(liveFailureOperationMeasurementWrite, 1+i%7, failureErr)
	}

	if lines := logLines.take(); len(lines) != 1 {
		t.Fatalf("10000 failures within one log interval logged %d lines at the default verbosity, want 1", len(lines))
	}
}
