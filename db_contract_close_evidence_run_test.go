// A close evidence row retention run through its steps, without a database or
// Redis: why it stops, what it counts, how it reads the brake and where it
// leaves its cursor.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
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

// A brake that cannot be read deletes nothing more in that run, and waits for
// the next run where it stood, whether it fails before the first batch or
// after some: the next run resumes after the last key read.
func TestCloseEvidenceRemovalStopsAtAnUnreadBrake(t *testing.T) {
	ctx := context.Background()
	now := NowUtc()
	settings := DefaultCloseEvidenceRetentionSettings()
	settings.BatchSize = 2
	full := testCloseEvidenceFullBatch(settings, now.Add(-40*24*time.Hour))
	unread := testCloseEvidenceBrake{err: errors.New("redis is down")}
	soon := time.Now().Add(time.Minute)

	steps := &testCloseEvidenceSteps{
		targets:   []string{closeEvidenceTable},
		retention: 45 * 24 * time.Hour,
		brakes:    []testCloseEvidenceBrake{unread},
		batches:   []testCloseEvidenceBatchOutcome{{batch: full}},
	}
	report, cursor, more := steps.run(ctx, now, CloseEvidenceRemovalCursor{}, soon, settings)
	connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopBrakeUnread)
	connect.AssertEqual(t, report.Removed, int64(0))
	connect.AssertEqual(t, more, false)
	connect.AssertEqual(t, len(steps.batchArgs), 0)

	steps = &testCloseEvidenceSteps{
		targets:   []string{closeEvidenceTable},
		retention: 45 * 24 * time.Hour,
		brakes:    []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(5000), unread},
		batches:   []testCloseEvidenceBatchOutcome{{batch: full}, {batch: full}},
	}
	report, cursor, more = steps.run(ctx, now, cursor, soon, settings)
	connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopBrakeUnread)
	connect.AssertEqual(t, report.Removed, int64(settings.BatchSize))
	connect.AssertEqual(t, more, false)
	connect.AssertEqual(t, len(steps.batchArgs), 1)
	connect.AssertEqual(t, cursor.InProgress(), true)

	steps = &testCloseEvidenceSteps{
		targets:   []string{closeEvidenceTable},
		retention: 45 * 24 * time.Hour,
		brakes:    []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(5000)},
	}
	report, _, _ = steps.run(ctx, now, cursor, soon, settings)
	connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopDone)
	connect.AssertEqual(t, steps.batchArgs[0].after, full.last)
}

// The brake is read before every batch, not once a run: turned off between
// batches, it stops the next one, and a rate lowered between batches paces the
// batch it was read for.
func TestCloseEvidenceRemovalReadsTheBrakeBeforeEveryBatch(t *testing.T) {
	ctx := context.Background()
	now := NowUtc()
	settings := DefaultCloseEvidenceRetentionSettings()
	settings.BatchSize = 2
	full := testCloseEvidenceFullBatch(settings, now.Add(-40*24*time.Hour))
	soon := time.Now().Add(time.Minute)
	batches := []testCloseEvidenceBatchOutcome{{batch: full}, {batch: full}, {batch: full}, {batch: full}}

	steps := &testCloseEvidenceSteps{
		targets:   []string{closeEvidenceTable},
		retention: 45 * 24 * time.Hour,
		brakes:    []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(5000), testCloseEvidenceBrakeOn(5000), testCloseEvidenceBrakeOff()},
		batches:   batches,
	}
	report, cursor, more := steps.run(ctx, now, CloseEvidenceRemovalCursor{}, soon, settings)
	connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopBrakeOff)
	connect.AssertEqual(t, report.Removed, int64(2*settings.BatchSize))
	connect.AssertEqual(t, more, false)
	connect.AssertEqual(t, steps.brakeReads, 3)
	connect.AssertEqual(t, len(steps.batchArgs), 2)
	connect.AssertEqual(t, cursor.InProgress(), true)

	steps = &testCloseEvidenceSteps{
		targets:   []string{closeEvidenceTable},
		retention: 45 * 24 * time.Hour,
		brakes:    []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(5000), testCloseEvidenceBrakeOn(0.001)},
		batches:   batches,
	}
	report, _, more = steps.run(ctx, now, CloseEvidenceRemovalCursor{}, soon, settings)
	connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopBudget)
	connect.AssertEqual(t, report.Removed, int64(2*settings.BatchSize))
	connect.AssertEqual(t, more, true)
	connect.AssertEqual(t, steps.brakeReads, 2)
	connect.AssertEqual(t, len(steps.batchArgs), 2)
}

// The retention lengthens during a pass, when the reaper's mode goes off or
// its horizon goes up. The resumed pass reads past its new upper bound, which
// is nothing, so it ends without deleting, and its low water falls to the new
// bound. When the retention shortens again that day, the next pass starts
// there, below where the first pass stood, so no row is left unread.
func TestCloseEvidenceRemovalLengthenedMidPass(t *testing.T) {
	ctx := context.Background()
	now := NowUtc()
	const day = 24 * time.Hour
	settings := DefaultCloseEvidenceRetentionSettings()
	soon := time.Now().Add(time.Minute)
	longUpper := ContractIdFloor(now.Add(-ContractRetention))
	shortUpper := ContractIdFloor(now.Add(-35 * day))
	today := now.UTC().Format(time.DateOnly)
	at := closeEvidenceKey{contractId: ContractIdFloor(now.Add(-40 * day)), clientId: NewId(), reportId: NewId()}
	inProgress := CloseEvidenceRemovalCursor{FullPassDay: today}.at(closeEvidenceTable, at)

	steps := &testCloseEvidenceSteps{
		targets:   []string{closeEvidenceTable},
		retention: ContractRetention,
		brakes:    []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(5000)},
	}
	report, cursor, more := steps.run(ctx, now, inProgress, soon, settings)
	connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopDone)
	connect.AssertEqual(t, report.Removed, int64(0))
	connect.AssertEqual(t, report.Retention, ContractRetention)
	connect.AssertEqual(t, more, false)
	connect.AssertEqual(t, steps.batchArgs[0].after, at)
	connect.AssertEqual(t, steps.batchArgs[0].upper, longUpper)
	connect.AssertEqual(t, steps.batchArgs[0].retention, ContractRetention)
	connect.AssertEqual(t, cursor.InProgress(), false)
	connect.AssertEqual(t, *cursor.LowWater, longUpper)

	steps = &testCloseEvidenceSteps{
		targets:   []string{closeEvidenceTable},
		retention: 35 * day,
		brakes:    []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(5000)},
	}
	report, _, _ = steps.run(ctx, now, cursor, soon, settings)
	connect.AssertEqual(t, report.FullPass, false)
	connect.AssertEqual(t, steps.batchArgs[0].after, closeEvidenceKeyBefore(&longUpper))
	connect.AssertEqual(t, steps.batchArgs[0].upper, shortUpper)
	connect.AssertEqual(t, steps.batchArgs[0].retention, 35*day)

	// a row the pass kept below the new bound holds the low water there, and
	// one above it does not raise the low water past the bound
	keptBelow := ContractIdFloor(now.Add(-100 * day))
	keptAbove := ContractIdFloor(now.Add(-50 * day))
	for _, test := range []struct {
		kept     Id
		lowWater Id
	}{
		{kept: keptBelow, lowWater: keptBelow},
		{kept: keptAbove, lowWater: longUpper},
	} {
		kept := test.kept
		resumed := inProgress
		resumed.PassLowWater = &kept
		steps = &testCloseEvidenceSteps{
			targets:   []string{closeEvidenceTable},
			retention: ContractRetention,
			brakes:    []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(5000)},
		}
		_, cursor, _ = steps.run(ctx, now, resumed, soon, settings)
		connect.AssertEqual(t, *cursor.LowWater, test.lowWater)
	}
}

// A transaction that records its statements and answers the batch's one read.
type testCloseEvidenceBatchTx struct {
	PgTx
	statements []string
	readArgs   []any
	last       closeEvidenceKey
	lowWater   Id
}

func (self *testCloseEvidenceBatchTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	self.statements = append(self.statements, sql)
	return pgconn.CommandTag{}, nil
}

func (self *testCloseEvidenceBatchTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	self.statements = append(self.statements, sql)
	self.readArgs = args
	return testCloseEvidenceBatchRow{tx: self}
}

type testCloseEvidenceBatchRow struct {
	tx *testCloseEvidenceBatchTx
}

func (self testCloseEvidenceBatchRow) Scan(dest ...any) error {
	*dest[0].(*int64) = 3
	*dest[1].(*int64) = 5
	*dest[2].(**Id) = &self.tx.last.contractId
	*dest[3].(**Id) = &self.tx.last.clientId
	*dest[4].(**Id) = &self.tx.last.reportId
	*dest[5].(**Id) = &self.tx.lowWater
	return nil
}

// A batch sets its statement and lock bounds and the guard's opt-in, from the
// settings, before its statement, and reads its answer; without the bounds a
// slow batch holds the transaction horizon or queues behind a lock
// (TestCloseEvidenceRetentionBatchBoundsItsStatement and
// TestCloseEvidenceRetentionBatchBoundsItsLockWait run them against
// PostgreSQL). The defaults are the contract horizon reaper's.
func TestCloseEvidenceRetentionBatchSetsItsBoundsBeforeItsStatement(t *testing.T) {
	defaults := DefaultCloseEvidenceRetentionSettings()
	connect.AssertEqual(t, defaults.StatementTimeout, 30*time.Second)
	connect.AssertEqual(t, defaults.LockTimeout, 2*time.Second)

	settings := DefaultCloseEvidenceRetentionSettings()
	settings.StatementTimeout = 31 * time.Second
	settings.LockTimeout = 3 * time.Second
	tx := &testCloseEvidenceBatchTx{
		last:     closeEvidenceKey{contractId: NewId(), clientId: NewId(), reportId: NewId()},
		lowWater: NewId(),
	}
	args := closeEvidenceBatchArgs{
		leaf:      "a_leaf",
		after:     closeEvidenceKey{contractId: NewId(), clientId: NewId(), reportId: NewId()},
		upper:     NewId(),
		retention: 45 * 24 * time.Hour,
		size:      7,
	}
	batch := removeCloseEvidenceBatchIn(context.Background(), tx, args, settings)

	want := []string{
		"SET LOCAL statement_timeout = '31000ms'",
		"SET LOCAL lock_timeout = '3000ms'",
		"SET LOCAL urnetwork.close_evidence_retention = 'on'",
	}
	if len(tx.statements) != len(want)+1 || !reflect.DeepEqual(tx.statements[:len(want)], want) {
		t.Fatalf("the batch ran %q, want %q and then its statement", tx.statements, want)
	}
	if !strings.Contains(tx.statements[len(want)], `DELETE FROM ONLY public."a_leaf"`) {
		t.Fatalf("the batch's last statement is not its delete:\n%s", tx.statements[len(want)])
	}
	if !reflect.DeepEqual(tx.readArgs, []any{args.after.contractId, args.after.clientId, args.after.reportId, args.upper, args.size, float64(45 * 24 * 60 * 60)}) {
		t.Fatalf("the batch read with %v", tx.readArgs)
	}
	connect.AssertEqual(t, batch.removed, int64(3))
	connect.AssertEqual(t, batch.scanned, int64(5))
	connect.AssertEqual(t, batch.last, tx.last)
	connect.AssertEqual(t, *batch.lowWater, tx.lowWater)
}

// The contract task's arguments carry the row retention's cursor. Arguments
// from before 2026-11-02 resume their pass at its contract, the next arguments
// hold evidence_removal alone, and every field survives the trip through JSON.
func TestContractRetentionCursorCarriesTheEvidenceRemoval(t *testing.T) {
	now := NowUtc()
	contractId := ContractIdFloor(now.Add(-100 * 24 * time.Hour))
	contractIdJson, err := json.Marshal(contractId)
	if err != nil {
		t.Fatal(err)
	}

	var legacy ContractRetentionCursor
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"evidence_cursor":%s,"evidence_leaf":"contract_close_report_evidence_legacy","analyzed_week":"2026-10-26"}`, contractIdJson)), &legacy); err != nil {
		t.Fatal(err)
	}
	removal := legacy.evidenceRemovalCursor()
	connect.AssertEqual(t, removal.InProgress(), true)
	connect.AssertEqual(t, removal.Leaf, "contract_close_report_evidence_legacy")
	connect.AssertEqual(t, *removal.ContractId, contractId)
	start, after, _ := removal.startIn([]string{"contract_close_report_evidence_default", "contract_close_report_evidence_legacy"}, now)
	connect.AssertEqual(t, start, 1)
	connect.AssertEqual(t, after, closeEvidenceKeyBefore(&contractId))

	// no old pass, no new cursor: a new pass
	var fresh ContractRetentionCursor
	if err := json.Unmarshal([]byte(`{}`), &fresh); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, fresh.evidenceRemovalCursor().InProgress(), false)

	clientId, reportId, floor, passLow, low, other := NewId(), NewId(), NewId(), NewId(), NewId(), NewId()
	full := CloseEvidenceRemovalCursor{
		Leaf:         "contract_close_report_evidence_legacy",
		ContractId:   &contractId,
		ClientId:     &clientId,
		ReportId:     &reportId,
		PassFloor:    &floor,
		PassLowWater: &passLow,
		LowWater:     &low,
		FullPassDay:  "2026-11-02",
	}
	next := legacy.withEvidenceRemoval(full)
	encoded, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"evidence_cursor"`, `"evidence_leaf"`} {
		if strings.Contains(string(encoded), field) {
			t.Errorf("the next arguments keep %s: %s", field, encoded)
		}
	}
	var decoded ContractRetentionCursor
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.evidenceRemovalCursor(), full) {
		t.Errorf("the cursor came back as %+v, want %+v", decoded.evidenceRemovalCursor(), full)
	}
	// the rest of the task's cursor stays
	connect.AssertEqual(t, decoded.AnalyzedWeek, "2026-10-26")

	// evidence_removal wins over old fields beside it
	both := ContractRetentionCursor{EvidenceCursor: &other, EvidenceLeaf: "another", EvidenceRemoval: &full}
	if !reflect.DeepEqual(both.evidenceRemovalCursor(), full) {
		t.Errorf("the old fields won: %+v", both.evidenceRemovalCursor())
	}
}

// A batch reads a minute of a rate too low for whole batches, since the next
// run's first batch follows a minute after a run stops: one batch a minute of
// that size is the rate, where whole batches would be about 83 rows a second
// at any rate below it. At the default rate and above, batches stay whole.
func TestCloseEvidenceRetentionBatchSizeFollowsALowRate(t *testing.T) {
	settings := DefaultCloseEvidenceRetentionSettings()
	for _, test := range []struct {
		rate float64
		size int
	}{
		{rate: settings.MaxRowsPerSecond, size: 5000},
		{rate: settings.DefaultRowsPerSecond, size: 5000},
		{rate: 84, size: 5000},
		{rate: 83, size: 4980},
		{rate: 50, size: 3000},
		{rate: 10, size: 600},
		{rate: 1, size: 60},
		{rate: 0.1, size: 6},
		// below a row a minute, a row a minute
		{rate: 0.001, size: 1},
	} {
		if size := settings.batchSizeAt(test.rate); size != test.size {
			t.Errorf("at %g rows a second a batch reads %d rows, want %d", test.rate, size, test.size)
		}
	}

	// each batch of a run takes its size from the brake read before it, and a
	// batch that fills that size is not the target's last
	short := DefaultCloseEvidenceRetentionSettings()
	short.BatchSize = 100
	short.MaxBatchWait = 10 * time.Millisecond
	now := NowUtc()
	steps := &testCloseEvidenceSteps{
		targets:   []string{closeEvidenceTable},
		retention: 45 * 24 * time.Hour,
		brakes:    []testCloseEvidenceBrake{testCloseEvidenceBrakeOn(1000), testCloseEvidenceBrakeOn(2000), testCloseEvidenceBrakeOn(5000)},
		batches: []testCloseEvidenceBatchOutcome{
			{batch: closeEvidenceBatch{removed: 10, scanned: 10, last: closeEvidenceKey{contractId: ContractIdFloor(now.Add(-40 * 24 * time.Hour))}}},
			{batch: closeEvidenceBatch{removed: 20, scanned: 20, last: closeEvidenceKey{contractId: ContractIdFloor(now.Add(-39 * 24 * time.Hour))}}},
			{batch: closeEvidenceBatch{removed: 49, scanned: 49}},
		},
	}
	report, _, more := steps.run(context.Background(), now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute), short)
	sizes := []int{}
	for _, args := range steps.batchArgs {
		sizes = append(sizes, args.size)
	}
	if !reflect.DeepEqual(sizes, []int{10, 20, 50}) {
		t.Fatalf("the batches read %v rows", sizes)
	}
	connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopDone)
	connect.AssertEqual(t, report.Removed, int64(79))
	connect.AssertEqual(t, more, false)
}
