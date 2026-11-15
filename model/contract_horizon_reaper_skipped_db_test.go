// The contract horizon reaper's count of what its pass moved past and kept:
// carried in the cursor from run to run, exported by reason, and restarted
// with each pass.
package model

import (
	"maps"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

// The value the process exports for the gauge name with this reason label, as
// a scrape reads it. False when there is none.
func horizonTestReasonGauge(name string, reason string) (float64, bool) {
	// a collector that fails elsewhere still leaves this family in the result
	families, _ := prometheus.DefaultGatherer.Gather()
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "reason" && label.GetValue() == reason && metric.GetGauge() != nil {
					return metric.GetGauge().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// The pass's skipped counts as the process exports them, every reason.
func checkHorizonTestSkippedExported(t testing.TB, want map[string]int64) {
	t.Helper()
	for _, reason := range contractHorizonProtectedReasons {
		if exported, ok := horizonTestReasonGauge("urnetwork_contract_horizon_pass_skipped_contracts", reason); !ok || exported != float64(want[reason]) {
			t.Errorf("exported %s skipped %v (found %t), want %d", reason, exported, ok, want[reason])
		}
	}
}

// The pass counts the contracts it moves past and keeps, by reason, in its
// cursor, so the count carries from one run to the next, on whichever
// taskworker runs it, and it exports them: a contract that closed 30 days
// after it was minted, longer than the close settle time covers
// (recent_close), and unsettled ones. A dry run counts nothing into the
// cursor, and a new pass starts from none.
func TestContractHorizonPassCountsWhatItMovesPast(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer removeHorizonTestControl(t, ctx)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		now := server.NowUtc()
		settings := horizonTestSettings()
		settings.Horizon = ContractHorizonFloor
		unsettledAt := func(createTime time.Time) *horizonTestContract {
			contract := newHorizonTestContract(createTime, "canceled")
			contract.settled = false
			return contract
		}

		closedLate := newHorizonTestContract(now.Add(-40*day), "canceled")
		closedLate.closeTime = horizonTestPointer(now.Add(-10 * day))
		eligible := newHorizonTestContract(now.Add(-40*day+time.Second), "canceled")
		insertHorizonTestContracts(t, ctx, f, closedLate, eligible, unsettledAt(now.Add(-39*day)))
		report, next, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		want := map[string]int64{
			contractHorizonProtectedRecentClose: 1,
			contractHorizonProtectedUnsettled:   1,
		}
		if err != nil || report.Eligible != 1 || !maps.Equal(next.SkippedReasonCounts, want) {
			t.Fatalf("the pass did not count what it moved past: %+v, cursor %+v, %v", report, next, err)
		}
		checkHorizonTestSkippedExported(t, want)

		insertHorizonTestContracts(t, ctx, f, unsettledAt(now.Add(-38*day)))
		report, next, err = RemoveContractsPastHorizon(ctx, now, next, settings)
		want[contractHorizonProtectedUnsettled] = 2
		if err != nil || report.Scanned != 1 || !maps.Equal(next.SkippedReasonCounts, want) {
			t.Fatalf("a later run of the pass did not add to its count: %+v, cursor %+v, %v", report, next, err)
		}
		checkHorizonTestSkippedExported(t, want)

		insertHorizonTestContracts(t, ctx, f, unsettledAt(now.Add(-37*day)))
		setHorizonTestControl(t, ctx, map[string]string{ContractHorizonControlFieldMode: "dry_run"})
		report, dryRunNext, err := RemoveContractsPastHorizon(ctx, now, next, settings)
		if err != nil || report.Protected[contractHorizonProtectedUnsettled] != 1 || !maps.Equal(dryRunNext.SkippedReasonCounts, want) {
			t.Fatalf("a dry run counted into the cursor: %+v, cursor %+v, %v", report, dryRunNext, err)
		}
		checkHorizonTestSkippedExported(t, want)
		removeHorizonTestControl(t, ctx)

		// the pass reads the last one, reaches the horizon and starts over
		settings.RescanInterval = time.Hour
		report, restarted, err := RemoveContractsPastHorizon(ctx, now.Add(2*time.Hour), next, settings)
		if err != nil || report.Protected[contractHorizonProtectedUnsettled] != 1 || !report.CaughtUp ||
			!restarted.PassStartTime.Equal(now.Add(2*time.Hour)) || len(restarted.SkippedReasonCounts) != 0 {
			t.Fatalf("a new pass did not start from no count: %+v, cursor %+v, %v", report, restarted, err)
		}
		checkHorizonTestSkippedExported(t, map[string]int64{})
		if !maps.Equal(next.SkippedReasonCounts, want) {
			t.Fatal("a run changed the counts of the cursor it was given", next.SkippedReasonCounts)
		}
	})
}
