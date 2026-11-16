package sidecarrestore

// §19.10's billing step (OPS-M6-01; D-OPS-14, FS-42, FI-13(b)): after the rows step, taskworker's meter rows newer
// than the sidecar dump are recomputed from network_service_usage and sent again, §20.9's daily rows replacing,
// never adding: model.RecomputeStripeMeterReports (QUOTA-M6-03) writes each closed Team day since the dump's day as
// a new version, queued, unless the day's newest version already holds the same value under a version a recompute
// since the dump wrote, so the step may run again and writes nothing more; taskworker's ReportStripeMeters sends
// them in day order, each a version Stripe keeps as the day's newest. Debits lost with the dump go unbilled: nothing
// here reconstructs them.
//
// A restored database whose dumps predate MG6 holds no stripe_meter_report, and the step has nothing to recompute;
// one that holds it under a binary that does not link MG6 is refused.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The list's rows the billing step writes.
const (
	// the recompute's counts: table stripe_meter_report, row the incident, detail what it wrote since when
	ListKindBillingRecomputed = "billing-recomputed"
)

// The table that says MG6 came back with the dumps.
const m6BillingTable = "stripe_meter_report"

// The restored database holds MG6's tables and this binary does not link MG6.
var ErrM6Unlinked = errors.New("the restored database holds MG6's tables, which this binary does not link")

// What the billing step runs on. A nil field takes the real one.
type BillingStepDeps struct {
	// Whether the restored database holds table.
	TablePresent func(ctx context.Context, table string) (bool, error)
	// Whether this binary links MG6.
	Linked func() bool
	// model.RecomputeStripeMeterReports.
	Recompute func(ctx context.Context, sinceTime time.Time) (model.RecomputeStripeMeterReportsResult, error)
	// Whether to check the database's restore marker: always, but for a test with no database.
	SkipMarker bool
}

// The billing step (see the file header) under run's incident.
func RunBillingStep(ctx context.Context, deps *BillingStepDeps, run *StepRun) error {
	if deps == nil {
		deps = &BillingStepDeps{}
	}
	incident := run.Incident
	if !deps.SkipMarker {
		if err := CheckRestoreMarker(ctx, incident.IncidentId()); err != nil {
			return err
		}
	}
	tablePresent := deps.TablePresent
	if tablePresent == nil {
		tablePresent = restoredTablePresent
	}
	linked := deps.Linked
	if linked == nil {
		linked = server.SidecarMg6Linked
	}
	recompute := deps.Recompute
	if recompute == nil {
		recompute = model.RecomputeStripeMeterReports
	}
	present, err := tablePresent(ctx, m6BillingTable)
	if err != nil {
		return err
	}
	if !present {
		fmt.Fprintf(run.Out, "no %s in the restored database, which predates MG6: the billing step has nothing to recompute\n", m6BillingTable)
		return appendOnce(incident, &ListRow{Kind: ListKindM5StepSkipped, Table: StepBilling, RowId: incident.IncidentId().String(), Detail: "no " + m6BillingTable + ": the database predates MG6"})
	}
	if !linked() {
		return fmt.Errorf("%w: run the billing step from a sidecarctl that links MG6", ErrM6Unlinked)
	}
	sidecarTime, _, err := incident.SnapshotTimes()
	if err != nil {
		return err
	}
	result, err := recompute(ctx, sidecarTime)
	if err != nil {
		return fmt.Errorf("the meter rows: %w", err)
	}
	detail := fmt.Sprintf("since %s: %d Team days, %d rolled up, %d rows written, %d past their deadline",
		sidecarTime.UTC().Format(time.RFC3339Nano), result.DayCount, result.RolledUpCount, result.WrittenCount, result.ClosedCount)
	// each run's counts are listed: a rerun that writes nothing lists its zero, which says it found the rows held
	if err := incident.Append(&ListRow{Kind: ListKindBillingRecomputed, Table: m6BillingTable, RowId: incident.IncidentId().String(), Detail: detail}); err != nil {
		return err
	}
	fmt.Fprintf(run.Out, "the billing step is done: %s; taskworker's ReportStripeMeters sends them in day order, and debits lost with the dump go unbilled\n", detail)
	return nil
}

// The step as the process's frame runs it, on the env WARP_ENV names.
func init() {
	RegisterStep(StepBilling, func(ctx context.Context, run *StepRun) error {
		return RunBillingStep(ctx, nil, run)
	})
}
