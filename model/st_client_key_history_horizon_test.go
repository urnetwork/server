package model

// The key history retention follows the contract horizon reaper's runtime
// control. DB-backed: the guard below 91 days is migration 904's, so under the
// owner rule this runs only after the branch is merged to main; on a database
// whose guard keeps 91 days it fails, as on the code before the change.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Sets the contract horizon reaper's control for the test's Redis database.
func setGoneKeyHistoryTestContractHorizon(t testing.TB, ctx context.Context, fields map[string]string) {
	t.Helper()
	controlCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if fields == nil {
		if err := RemoveContractHorizonControl(controlCtx); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := SetContractHorizonControl(controlCtx, fields); err != nil {
		t.Fatal(err)
	}
}

// The retention follows the contract horizon reaper's control: at a 45-day
// horizon a client gone 50 days loses its history and one gone 40 days keeps
// it, and without the control the 91-day contract retention keeps both.
func TestGoneStClientKeyHistoryFollowsContractHorizon(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		keptId, removedId, laterId := server.NewId(), server.NewId(), server.NewId()
		for _, clientId := range []server.Id{keptId, removedId, laterId} {
			insertGoneKeyHistoryTestRows(t, ctx, clientId, server.NewId())
		}
		setGoneKeyHistoryTestDays(t, ctx, keptId, 40)
		setGoneKeyHistoryTestDays(t, ctx, removedId, 50)

		setGoneKeyHistoryTestContractHorizon(t, ctx, nil)
		now := goneKeyHistoryTestDbNow(t, ctx)
		report, _ := RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
		if report.RemovedClientCount != 0 {
			t.Fatalf("without a control the retention = %+v, want none removed", report)
		}
		for _, clientId := range []server.Id{keptId, removedId} {
			if rows := readGoneKeyHistoryTestRows(t, ctx, clientId); rows.history != 1 || rows.heads != 1 {
				t.Fatalf("without a control a client gone under 91 days lost key history: %+v", rows)
			}
		}

		setGoneKeyHistoryTestContractHorizon(t, ctx, map[string]string{ContractHorizonControlFieldHorizonDays: "45"})
		now = goneKeyHistoryTestDbNow(t, ctx)
		report, _ = RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
		if report.RemovedClientCount != 1 || report.RemovedHistoryCount != 1 {
			t.Fatalf("at a 45-day horizon the retention = %+v, want one client", report)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, keptId); rows != (goneKeyHistoryTestRows{history: 1, heads: 1, gone: true}) {
			t.Fatalf("a client gone 40 days lost key history at a 45-day horizon: %+v", rows)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, removedId); rows != (goneKeyHistoryTestRows{}) {
			t.Fatalf("a client gone 50 days kept key history at a 45-day horizon: %+v", rows)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, laterId); rows.history != 1 || rows.heads != 1 {
			t.Fatalf("a client that has not gone lost key history: %+v", rows)
		}
	})
}
