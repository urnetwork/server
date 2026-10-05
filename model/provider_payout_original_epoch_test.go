// Original epoch evidence and asset selection have different intervals. These
// fixtures exercise the actual SQL census, archive custody and legacy planner.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urnetwork/server"
)

// A straddling epoch retains the original pre-cutoff row while equal completed
// paid/free bytes at and after the boundary receive equal mainnet weight.
func TestProviderTransitionWholeCensusRetainsOriginalEpochAndSelectsNewEarnings(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		paid, free := newPayoutTransitionCohort(t, ctx), newPayoutTransitionCohort(t, ctx)
		start, end := payoutTestCutoff.Add(-time.Hour), payoutTestCutoff.Add(time.Hour)
		before, at, after := payoutTestCutoff.Add(-time.Microsecond), payoutTestCutoff, payoutTestCutoff.Add(time.Microsecond)
		legacy := paid.insert(t, ctx, start, &before, end.Add(time.Hour), 11, UsdToNanoCents(1))
		paid.insert(t, ctx, start, &at, end.Add(time.Hour), 1024, UsdToNanoCents(9))
		freeContract := free.insert(t, ctx, start, &after, end.Add(time.Hour), 1024, 0)
		free.insert(t, ctx, start, &end, end.Add(time.Hour), 4096, 0)
		usages, census, window, err := GetStEpochProviderUsageWholeCensus(ctx, 7, start, end)
		if err != nil || census == nil || window == nil {
			t.Fatal("original straddling epoch census unavailable", err)
		}
		if len(usages) != 2 || usages[0].PayoutByteCount != 1024 || usages[1].PayoutByteCount != 1024 {
			t.Fatal("cutoff or paid/free earning weight changed", usages)
		}
		if census.Count != 3 || len(census.Records) != 3 || len(window.Records) != 3 || census.WindowStart != start.Format(time.RFC3339Nano) || census.WindowEnd != end.Format(time.RFC3339Nano) {
			t.Fatal("earning filter clipped original census", census, window)
		}
		selection, err := GetProviderPayoutEarningSelection(ctx)
		if err != nil || selection == nil || !selection.StartTime.Equal(payoutTestCutoff) || census.EarningStart != payoutTestCutoff.Format(time.RFC3339Nano) || census.EarningSelectionHash != selection.PolicyHash {
			t.Fatal("census selected earnings without independent immutable policy", selection, err)
		}
		windowStart, startErr := time.Parse(time.RFC3339Nano, window.Start)
		windowEnd, endErr := time.Parse(time.RFC3339Nano, window.End)
		if startErr != nil || endErr != nil || !windowStart.Equal(start) || !windowEnd.Equal(end) {
			t.Fatal("same statement window lost original epoch clock", startErr, endErr)
		}
		var original []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, legacy).Scan(&original))
		})
		retained := false
		for _, row := range census.Records {
			if row.ContractId == [16]byte(legacy) {
				retained = bytes.Equal(row.Original, original) && row.ClosedAt == before.Format(time.RFC3339Nano)
			}
		}
		if !retained {
			t.Fatal("pre-cutoff original was omitted or reconstructed")
		}
		var reports payoutartifact.ClosedWorkReports
		if err := json.Unmarshal(census.Records[0].OriginalReports, &reports); err != nil || reports.Window == nil || payoutartifact.SnapshotHash(reports.Window) != payoutartifact.SnapshotHash(window) {
			t.Fatal("report companion lost full same-statement window", err)
		}
		plan, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil || len(plan.NetworkPayments) != 1 || plan.NetworkPayments[paid.network] == nil || plan.NetworkPayments[paid.network].PayoutByteCount != 11 {
			t.Fatal("original pre-cutoff USDC debt changed asset", plan, err)
		}
		if err := RequireProviderUsdcPayment(ctx, plan.NetworkPayments[paid.network].PaymentId); err != nil {
			t.Fatal("retained pre-cutoff USDC debt became unpayable", err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, freeContract))
		})
		afterUsages, afterCensus, afterWindow, err := GetStEpochProviderUsageWholeCensus(ctx, 7, start, end)
		if err != nil || afterCensus == nil || afterWindow == nil || afterCensus.Hash() != census.Hash() || payoutartifact.SnapshotHash(afterWindow) != payoutartifact.SnapshotHash(window) || len(afterUsages) != 2 || afterUsages[0].PayoutByteCount != 1024 || afterUsages[1].PayoutByteCount != 1024 {
			t.Fatal("archive custody changed original evidence or earning selection", err)
		}
	})
}
