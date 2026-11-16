package sidecarrestore

// §19.10's billing step in the frame (OPS-M6-01): after the rows step, it recomputes the meter rows from the sidecar
// dump's time, lists what each run wrote, skips a database before MG6 and refuses a binary without it. The recompute
// itself, a day's rows written again as a new version and nothing more on a rerun, is QUOTA-M6-03's
// (model.RecomputeStripeMeterReports), faked here.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// A recompute that writes a day's rows on its first call and finds them held after, as model's does.
type testRecompute struct {
	since []time.Time
}

func (self *testRecompute) recompute(ctx context.Context, sinceTime time.Time) (model.RecomputeStripeMeterReportsResult, error) {
	self.since = append(self.since, sinceTime)
	result := model.RecomputeStripeMeterReportsResult{DayCount: 2}
	if len(self.since) == 1 {
		result.WrittenCount = 3
	}
	return result, nil
}

// The billing step runs only once the rows step ended, recomputes from the sidecar dump's time, and a rerun lists
// that it wrote nothing more.
func TestBillingStepRecomputesFromTheSidecarDumpAfterTheRowsStep(t *testing.T) {
	recompute := &testRecompute{}
	frame := NewFrame()
	frame.Register(StepOps, testCountingStep(new(int)))
	frame.Register(StepBilling, func(ctx context.Context, run *StepRun) error {
		return RunBillingStep(ctx, &BillingStepDeps{
			TablePresent: func(ctx context.Context, table string) (bool, error) { return true, nil },
			Linked:       func() bool { return true },
			Recompute:    recompute.recompute,
			SkipMarker:   true,
		}, run)
	})
	incident := testIncident(t, IncidentKindPostgres)
	out := &bytes.Buffer{}
	connect.AssertEqual(t, frame.RunStep(t.Context(), incident, StepOps, out, ""), nil)
	connect.AssertEqual(t, errors.Is(frame.RunStep(t.Context(), incident, StepBilling, out, ""), ErrStepOutOfOrder), true)
	for _, step := range []string{StepAbuse, StepDns, StepApi, StepLabels, StepRows} {
		connect.AssertEqual(t, frame.EndStepByHand(incident, step, "by hand in this test"), nil)
	}
	connect.AssertEqual(t, frame.RunStep(t.Context(), incident, StepBilling, out, ""), nil)
	connect.AssertEqual(t, frame.RunStep(t.Context(), incident, StepBilling, out, ""), nil)
	connect.AssertEqual(t, len(recompute.since), 2)
	connect.AssertEqual(t, recompute.since[0].Equal(testSidecarDumpTime), true)
	rows := incident.RowsOfKind(ListKindBillingRecomputed)
	connect.AssertEqual(t, len(rows), 2)
	connect.AssertEqual(t, strings.Contains(rows[0].Detail, "3 rows written"), true)
	connect.AssertEqual(t, strings.Contains(rows[1].Detail, "0 rows written"), true)
	connect.AssertEqual(t, incident.StepEnded(StepBilling), true)
}

// A database before MG6 has no meter rows to recompute; one with them under a binary without MG6 is refused.
func TestBillingStepSkipsADatabaseBeforeMg6AndRefusesAnUnlinkedBinary(t *testing.T) {
	recompute := &testRecompute{}
	incident := testIncident(t, IncidentKindPostgres)
	run := &StepRun{Incident: incident, Out: &bytes.Buffer{}}
	absent := &BillingStepDeps{
		TablePresent: func(ctx context.Context, table string) (bool, error) { return false, nil },
		Recompute:    recompute.recompute,
		SkipMarker:   true,
	}
	connect.AssertEqual(t, RunBillingStep(t.Context(), absent, run), nil)
	connect.AssertEqual(t, len(incident.RowsOfKind(ListKindM5StepSkipped)), 1)
	unlinked := &BillingStepDeps{
		TablePresent: func(ctx context.Context, table string) (bool, error) { return true, nil },
		Linked:       func() bool { return false },
		Recompute:    recompute.recompute,
		SkipMarker:   true,
	}
	connect.AssertEqual(t, errors.Is(RunBillingStep(t.Context(), unlinked, run), ErrM6Unlinked), true)
	connect.AssertEqual(t, len(recompute.since), 0)
}

// The process's frame holds the billing step's function.
func TestBillingStepIsRegisteredInTheProcesssFrame(t *testing.T) {
	for _, step := range DefaultFrame().Steps() {
		if step.Name == StepBilling {
			connect.AssertEqual(t, step.Function != nil, true)
		}
	}
}
