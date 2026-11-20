package work

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

// A bounded maintenance response issued no claims. Drain the next page within
// the existing task instead of waiting through an unrelated task-error backoff.
func TestUrlCompletedSchedulerDrainsMaintenanceBeforeAdmission(t *testing.T) {
	pass, args, _ := testUrlProbePass()
	args.UrlProbe.Limit, args.UrlProbe.Concurrency = 1, 1
	calls := 0
	pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
		calls++
		if calls <= 3 {
			return nil, ingest.ErrDuePriorityMaintenancePending
		}
		if calls == 4 {
			return testDueProviders("synthetic-provider"), nil
		}
		return nil, nil
	}
	pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
		if calls != 4 || len(providers) != 1 {
			t.Error("opened a provider before exact priority maintenance finished")
		}
		for _, provider := range providers {
			if err := options.HealthResults.SubmitEgressHealth(ctx, provider.ClientId, testHealthRun(1, 0)); err != nil {
				return prober.Summary{}, err
			}
			if err := options.Attempts.ReportAttempt(ctx, provider.ClientId, ""); err != nil {
				return prober.Summary{}, err
			}
		}
		return prober.Summary{Attempted: len(providers), Submitted: len(providers)}, nil
	}
	result, err := pass.run(t.Context(), args)
	if err != nil || calls != 5 || result.Attempted != 1 || result.Submitted != 1 || result.Backlog {
		t.Fatalf("maintenance stalled or remained falsely pending after drain: calls=%d result=%+v error=%v", calls, result, err)
	}
}

// Prompt follow-up is not permission to ignore cancellation or open a tunnel
// while the API still reports an incomplete count snapshot.
func TestUrlCompletedSchedulerMaintenanceHonorsCancellation(t *testing.T) {
	pass, args, _ := testUrlProbePass()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
		calls++
		if calls == 3 {
			cancel()
		}
		return nil, ingest.ErrDuePriorityMaintenancePending
	}
	pass.runFull = func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
		t.Error("opened a tunnel during pending maintenance")
		return prober.Summary{}, nil
	}
	result, err := pass.run(ctx, args)
	if !errors.Is(err, context.Canceled) || calls != 3 || result.Attempted != 0 || !result.Backlog {
		t.Fatalf("maintenance ignored cancellation or became empty: calls=%d result=%+v error=%v", calls, result, err)
	}
}
