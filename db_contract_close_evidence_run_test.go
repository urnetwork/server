// A close evidence row retention run through its steps, without a database or
// Redis: why it stops, what it counts, how it reads the brake and where it
// leaves its cursor.
package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// One read of the brake.
type testCloseEvidenceBrake struct {
	control *CloseEvidenceRetentionControl
	err     error
}

func testCloseEvidenceBrakeOn(rate float64) testCloseEvidenceBrake {
	return testCloseEvidenceBrake{control: &CloseEvidenceRetentionControl{Present: true, On: true, MaxRowsPerSecond: rate}}
}

func testCloseEvidenceBrakeOff() testCloseEvidenceBrake {
	return testCloseEvidenceBrake{control: &CloseEvidenceRetentionControl{Present: true}}
}

// One batch's outcome.
type testCloseEvidenceBatchOutcome struct {
	batch closeEvidenceBatch
	err   error
	// runs during the batch
	during func()
}

// A run's steps from lists, recording the calls. A brake read past its list
// repeats the last one, and a batch past its list reads nothing, which ends
// its target.
type testCloseEvidenceSteps struct {
	targets      []string
	targetsErr   error
	retention    time.Duration
	retentionErr error
	brakes       []testCloseEvidenceBrake
	batches      []testCloseEvidenceBatchOutcome

	brakeReads int
	batchArgs  []closeEvidenceBatchArgs
}

func (self *testCloseEvidenceSteps) Targets(ctx context.Context, now time.Time) ([]string, error) {
	return self.targets, self.targetsErr
}

func (self *testCloseEvidenceSteps) Retention(ctx context.Context) (time.Duration, error) {
	return self.retention, self.retentionErr
}

func (self *testCloseEvidenceSteps) Brake(ctx context.Context, settings *CloseEvidenceRetentionSettings) (*CloseEvidenceRetentionControl, error) {
	read := self.brakes[min(self.brakeReads, len(self.brakes)-1)]
	self.brakeReads += 1
	return read.control, read.err
}

func (self *testCloseEvidenceSteps) Batch(ctx context.Context, args closeEvidenceBatchArgs, settings *CloseEvidenceRetentionSettings) (closeEvidenceBatch, error) {
	index := len(self.batchArgs)
	self.batchArgs = append(self.batchArgs, args)
	if len(self.batches) <= index {
		return closeEvidenceBatch{}, nil
	}
	outcome := self.batches[index]
	if outcome.during != nil {
		outcome.during()
	}
	return outcome.batch, outcome.err
}

// One run through the steps, as RemoveExpiredCloseEvidence makes it.
func (self *testCloseEvidenceSteps) run(ctx context.Context, now time.Time, cursor CloseEvidenceRemovalCursor, deadline time.Time, settings *CloseEvidenceRetentionSettings) (CloseEvidenceRemovalReport, CloseEvidenceRemovalCursor, bool) {
	removal := &closeEvidenceRemoval{
		settings: settings,
		steps:    self,
		pace:     newCloseEvidenceRetentionPace(),
		deadline: deadline,
	}
	return removal.run(ctx, now, cursor)
}

// A full batch of settings' size whose last key is in the contract at.
func testCloseEvidenceFullBatch(settings *CloseEvidenceRetentionSettings, at time.Time) closeEvidenceBatch {
	return closeEvidenceBatch{
		removed: int64(settings.BatchSize),
		scanned: int64(settings.BatchSize),
		last:    closeEvidenceKey{contractId: ContractIdFloor(at), clientId: NewId(), reportId: NewId()},
	}
}

var testCloseEvidenceRemovalStops = []string{
	CloseEvidenceRemovalStopDone,
	CloseEvidenceRemovalStopBudget,
	CloseEvidenceRemovalStopBrakeOff,
	CloseEvidenceRemovalStopBrakeUnread,
	CloseEvidenceRemovalStopCanceled,
	CloseEvidenceRemovalStopLockTimeout,
	CloseEvidenceRemovalStopStatementTimeout,
	CloseEvidenceRemovalStopError,
}

func testCloseEvidenceStopCounts() map[string]float64 {
	counts := map[string]float64{}
	for _, stop := range testCloseEvidenceRemovalStops {
		counts[stop] = testutil.ToFloat64(closeEvidenceRetentionStopCounter.WithLabelValues(stop))
	}
	return counts
}

// Every way a run ends has its own stop, and is counted once under it: a brake
// left off does not read as caught up, and a statement canceled, a lock wait
// that timed out and any other failure are told apart, however the error is
// wrapped.
func TestCloseEvidenceRemovalCountsWhyItStopped(t *testing.T) {
	now := NowUtc()
	settings := DefaultCloseEvidenceRetentionSettings()
	settings.BatchSize = 2
	full := testCloseEvidenceFullBatch(settings, now.Add(-40*24*time.Hour))
	targets := []string{closeEvidenceTable}
	failedPg := func(code string) error {
		return fmt.Errorf("the batch: %w", &pgconn.PgError{Code: code})
	}
	during, endDuring := context.WithCancel(context.Background())
	defer endDuring()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	soon := time.Now().Add(time.Minute)

	for _, test := range []struct {
		name     string
		ctx      context.Context
		steps    *testCloseEvidenceSteps
		deadline time.Time
		stop     string
		removed  int64
		more     bool
	}{
		{
			name:  "a pass that finished",
			steps: &testCloseEvidenceSteps{brakes: []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(1000)}},
			stop:  CloseEvidenceRemovalStopDone,
		},
		{
			name:     "no time left",
			steps:    &testCloseEvidenceSteps{brakes: []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(1000)}},
			deadline: time.Now().Add(-time.Second),
			stop:     CloseEvidenceRemovalStopBudget,
			more:     true,
		},
		{
			name: "a pace past the deadline",
			steps: &testCloseEvidenceSteps{
				brakes:  []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(0.001)},
				batches: []testCloseEvidenceBatchOutcome{{batch: full}},
			},
			stop:    CloseEvidenceRemovalStopBudget,
			removed: 2,
			more:    true,
		},
		{
			name:  "the brake off",
			steps: &testCloseEvidenceSteps{brakes: []testCloseEvidenceBrake{testCloseEvidenceBrakeOff()}},
			stop:  CloseEvidenceRemovalStopBrakeOff,
		},
		{
			name:  "the brake unread",
			steps: &testCloseEvidenceSteps{brakes: []testCloseEvidenceBrake{{err: errors.New("redis is down")}}},
			stop:  CloseEvidenceRemovalStopBrakeUnread,
		},
		{
			name:  "canceled before a batch",
			ctx:   canceled,
			steps: &testCloseEvidenceSteps{brakes: []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(1000)}},
			stop:  CloseEvidenceRemovalStopCanceled,
			more:  true,
		},
		{
			name: "canceled during a batch, which a canceled statement also reports",
			ctx:  during,
			steps: &testCloseEvidenceSteps{
				brakes:  []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(1000)},
				batches: []testCloseEvidenceBatchOutcome{{err: failedPg(pgerrcode.QueryCanceled), during: endDuring}},
			},
			stop: CloseEvidenceRemovalStopCanceled,
		},
		{
			name: "a lock wait past its timeout",
			steps: &testCloseEvidenceSteps{
				brakes:  []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(1000)},
				batches: []testCloseEvidenceBatchOutcome{{err: failedPg(pgerrcode.LockNotAvailable)}},
			},
			stop: CloseEvidenceRemovalStopLockTimeout,
		},
		{
			name: "a statement past its timeout",
			steps: &testCloseEvidenceSteps{
				brakes:  []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(1000)},
				batches: []testCloseEvidenceBatchOutcome{{err: failedPg(pgerrcode.QueryCanceled)}},
			},
			stop: CloseEvidenceRemovalStopStatementTimeout,
		},
		{
			name: "another database error",
			steps: &testCloseEvidenceSteps{
				brakes:  []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(1000)},
				batches: []testCloseEvidenceBatchOutcome{{err: failedPg(pgerrcode.RaiseException)}},
			},
			stop: CloseEvidenceRemovalStopError,
		},
		{
			name: "another error",
			steps: &testCloseEvidenceSteps{
				brakes:  []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(1000)},
				batches: []testCloseEvidenceBatchOutcome{{err: errors.New("the connection is closed")}},
			},
			stop: CloseEvidenceRemovalStopError,
		},
		{
			name:  "the targets unread",
			steps: &testCloseEvidenceSteps{targetsErr: errors.New("no connection"), brakes: []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(1000)}},
			stop:  CloseEvidenceRemovalStopError,
		},
		{
			name:  "the retention unread",
			steps: &testCloseEvidenceSteps{retentionErr: errors.New("no connection"), brakes: []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(1000)}},
			stop:  CloseEvidenceRemovalStopError,
		},
	} {
		ctx := test.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		deadline := test.deadline
		if deadline.IsZero() {
			deadline = soon
		}
		test.steps.targets = targets
		test.steps.retention = 45 * 24 * time.Hour
		before := testCloseEvidenceStopCounts()
		report, _, more := test.steps.run(ctx, now, CloseEvidenceRemovalCursor{}, deadline, settings)
		if report.Stop != test.stop || report.Removed != test.removed || more != test.more {
			t.Errorf("%s: stop %q, removed %d, more %t; want %q, %d, %t", test.name, report.Stop, report.Removed, more, test.stop, test.removed, test.more)
		}
		after := testCloseEvidenceStopCounts()
		for _, stop := range testCloseEvidenceRemovalStops {
			want := before[stop]
			if stop == test.stop {
				want += 1
			}
			if after[stop] != want {
				t.Errorf("%s: %d runs counted under %s, want %d", test.name, int(after[stop]-before[stop]), stop, int(want-before[stop]))
			}
		}
	}
}
