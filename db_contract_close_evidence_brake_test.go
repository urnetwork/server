// The close evidence row retention's brake, pace and guard floor, without a
// database.
package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A missing brake deletes at the code's default rate; a mode other than on, a
// rate of zero and a rate that does not parse stop the deletes; a rate above
// the code's ceiling is held to it.
func TestNarrowCloseEvidenceRetentionControl(t *testing.T) {
	settings := DefaultCloseEvidenceRetentionSettings()
	for _, test := range []struct {
		name   string
		fields map[string]string
		on     bool
		rate   float64
	}{
		{name: "absent", fields: map[string]string{}, on: true, rate: settings.DefaultRowsPerSecond},
		{name: "on", fields: map[string]string{"mode": "on"}, on: true, rate: settings.DefaultRowsPerSecond},
		{name: "off", fields: map[string]string{"mode": "off"}, on: false, rate: settings.DefaultRowsPerSecond},
		{name: "another mode", fields: map[string]string{"mode": "dry_run"}, on: false, rate: settings.DefaultRowsPerSecond},
		{name: "a rate", fields: map[string]string{"mode": "on", "maxRowsPerSecond": "250"}, on: true, rate: 250},
		{name: "a rate without a mode", fields: map[string]string{"maxRowsPerSecond": " 250 "}, on: true, rate: 250},
		{name: "on at zero", fields: map[string]string{"mode": "on", "maxRowsPerSecond": "0"}, on: false, rate: 0},
		{name: "a negative rate", fields: map[string]string{"maxRowsPerSecond": "-5"}, on: false, rate: -5},
		{name: "an unparsable rate", fields: map[string]string{"mode": "on", "maxRowsPerSecond": "fast"}, on: false, rate: 0},
		{name: "NaN", fields: map[string]string{"maxRowsPerSecond": "NaN"}, on: false, rate: 0},
		{name: "above the ceiling", fields: map[string]string{"maxRowsPerSecond": "1e9"}, on: true, rate: settings.MaxRowsPerSecond},
	} {
		control := NarrowCloseEvidenceRetentionControl(test.fields, settings)
		if control.On != test.on || control.MaxRowsPerSecond != test.rate || control.Present != (0 < len(test.fields)) {
			t.Errorf("%s: %+v, want on %t at %g", test.name, control, test.on, test.rate)
		}
	}
}

// A batch of n rows at r a second holds the next one n/r seconds after it
// began; a wait that would reach the deadline, or a canceled context, ends
// the run instead; a rate of zero never waits.
func TestCloseEvidenceRetentionPace(t *testing.T) {
	ctx := context.Background()
	pace := newCloseEvidenceRetentionPace()
	start := time.Now()
	if !pace.WaitAfter(ctx, 100, 1000, start.Add(time.Minute)) {
		t.Fatal("a 100 ms wait inside the deadline ended the run")
	}
	if waited := time.Since(start); waited < 90*time.Millisecond {
		t.Fatalf("100 rows at 1000 a second waited %s", waited)
	}
	if pace.WaitAfter(ctx, 1000, 10, time.Now().Add(time.Second)) {
		t.Fatal("a 100 s wait past the deadline did not end the run")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if pace.WaitAfter(canceled, 100, 10, time.Now().Add(time.Minute)) {
		t.Fatal("a canceled run waited")
	}
	before := time.Now()
	if !pace.WaitAfter(ctx, 1000000, 0, before) || 50*time.Millisecond < time.Since(before) {
		t.Fatal("a rate of zero waited")
	}
}

// The guard floor is 35 days only for the horizon guard's body. A read that
// failed, a missing function and any other body keep 91 days, so the retention
// a horizon of 35 days asks for stays 91.
func TestCloseEvidenceGuardFloorOf(t *testing.T) {
	horizonBody := ContractCloseEvidenceHorizonGuardBodySql()
	for _, test := range []struct {
		name  string
		body  string
		err   error
		floor time.Duration
	}{
		{name: "the horizon guard", body: horizonBody, floor: CloseEvidenceRetentionFloor},
		{name: "the move guard", body: ContractCloseEvidenceMoveGuardBodySql(), floor: ContractRetention},
		{name: "a missing function", body: "", floor: ContractRetention},
		{name: "a failed read", body: horizonBody, err: errors.New("canceling statement due to statement timeout"), floor: ContractRetention},
	} {
		if floor := closeEvidenceGuardFloorOf(test.body, test.err); floor != test.floor {
			t.Errorf("%s: floor %s, want %s", test.name, floor, test.floor)
		}
	}
	ctx := WithContractHorizon(context.Background(), CloseEvidenceRetentionFloor)
	if retention := closeEvidenceRetentionIn(ctx, testFailingQuerier{}); retention != ContractRetention {
		t.Fatalf("a guard read that fails keeps %s, want %s", retention, ContractRetention)
	}
}

// A querier whose every read fails.
type testFailingQuerier struct {
	PgQuerier
}

func (testFailingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return testFailingRow{}
}

type testFailingRow struct{}

func (testFailingRow) Scan(dest ...any) error {
	return errors.New("the connection is closed")
}
