package work

// The probe as the operator's half of the per-country blocked state
// (connect/EXTENDER.md R4, connect/EXTENDER-UPDATE2.md §1.3), end to end
// through the task: a failed probe is an outage, and the block reports made
// before it describe that outage, so none of them counts toward a block once
// the extender answers again.

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Root cause guarded: the tables keep only each address's latest probe, so a
// report sent in the minutes between an outage's start and the first probe
// that saw it was still counted once the probe succeeded again, and an
// extender that was simply down for a few minutes was blocked in the
// reporters' country for the rest of the report window.
func TestExtenderProbeFailureDiscountsTheBlockReportsBeforeIt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfig(t)
		extenderId := createTestExtender(ctx, 4, 4)
		settings := connect.DefaultExtenderBlockedStateSettings()
		blocked := func() bool {
			return model.NetworkExtenderBlockedInCountry(
				ctx,
				extenderId,
				"aa",
				settings.ReportThreshold,
				settings.ReportWindow,
				settings.ProbeWindow,
				server.NowUtc(),
			)
		}
		report := func() bool {
			return model.RecordNetworkExtenderBlockReport(ctx, extenderId, model.Testing_CreateBlockReportClient(ctx, server.NewId()), "aa", server.NowUtc())
		}
		failing := false
		stubExtenderProbe(t, func() error {
			if failing {
				return fmt.Errorf("the extender did not answer")
			}
			return nil
		})

		// reached by the probe, and reported by enough clients: a block
		connect.AssertEqual(t, runTestExtenderProbe(t, ctx).Succeeded, 1)
		for range settings.ReportThreshold {
			if !report() {
				t.Fatal("a report against an extender the probe reaches was not recorded")
			}
		}
		if !blocked() {
			t.Fatal("reports at the threshold, with the probe reaching the extender, are no block")
		}

		// the reports were made in the outage's first minutes, and the next
		// probe fails: an outage, which also sets them aside
		failing = true
		failed := runTestExtenderProbe(t, ctx)
		connect.AssertEqual(t, failed.Failed, 1)
		if blocked() || report() {
			t.Fatal("an extender failing its probe was blocked, or recorded a report")
		}

		// it answers again, and is not blocked by what was said of its outage
		failing = false
		answered := runTestExtenderProbe(t, ctx)
		connect.AssertEqual(t, answered.Succeeded, 1)
		if blocked() {
			t.Fatal("the reports made before an outage made a block after it")
		}
		// the failed probe counted what it set aside, and the next one nothing
		connect.AssertEqual(t, failed.DiscountedBlockReports, settings.ReportThreshold)
		connect.AssertEqual(t, answered.DiscountedBlockReports, 0)

		// reports made since it answers count as before
		for range settings.ReportThreshold {
			if !report() {
				t.Fatal("a report after the recovery was not recorded")
			}
		}
		if !blocked() {
			t.Fatal("reports at the threshold after the recovery are no block")
		}
	})
}
