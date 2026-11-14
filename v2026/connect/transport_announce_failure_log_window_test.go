package connect

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// After the interval the next failure logs again, with the failures by
// operation since the last line and the latest error; the interval restarts
// from that line.
func TestLiveFailureLogSummarizesEachInterval(t *testing.T) {
	requireDefaultVerbosityForTest(t)
	logLines := &logLinesForTest{}
	failureLog := newLiveFailureLog(logLines.logf)
	now := time.Date(2026, 10, 14, 17, 0, 0, 0, time.UTC)
	failureLog.now = func() time.Time {
		return now
	}

	failureLog.record(liveFailureOperationSyncRead, 1, errors.New("test: first"))
	if lines := logLines.take(); len(lines) != 1 || !strings.Contains(lines[0], "(first): sync_read=1;") {
		t.Fatalf("the first failure logged %q, want one first line", lines)
	}
	for i := 0; i < 5; i += 1 {
		failureLog.record(liveFailureOperationMeasurementWrite, i+1, errors.New("test: write"))
	}
	now = now.Add(30 * time.Second)
	failureLog.record(liveFailureOperationVerifyEgressLease, 1, errors.New("test: lease"))
	if lines := logLines.take(); len(lines) != 0 {
		t.Fatalf("failures within the interval logged %q", lines)
	}

	now = now.Add(30 * time.Second)
	failureLog.record(liveFailureOperationSyncRead, 2, errors.New("test: latest"))
	lines := logLines.take()
	if len(lines) != 1 {
		t.Fatalf("the failure after the interval logged %d lines, want 1", len(lines))
	}
	for _, want := range []string{
		"(in 1m0s): measurement_write=5 sync_read=1 verify_egress_lease=1;",
		"latest sync_read: test: latest",
	} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("the interval's line %q is missing %q", lines[0], want)
		}
	}

	now = now.Add(59 * time.Second)
	failureLog.record(liveFailureOperationSyncRead, 3, errors.New("test: too soon"))
	if lines := logLines.take(); len(lines) != 0 {
		t.Fatalf("a failure 59s after the last line logged %q", lines)
	}
}
