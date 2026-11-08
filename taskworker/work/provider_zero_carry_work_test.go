package work

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A run exports when it ran and what is in force, carries its place in the
// pass on, and a run that finds no connected provider completes the pass. A
// country whose last verdict cleared leaves the gauge.
func TestJudgeProviderZeroCarryExportsTheRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// a country left over from an earlier run
		providerZeroCarryProviders.WithLabelValues("kz").Set(14)

		now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		cursor := server.NewId()
		result := judgeProviderZeroCarry(ctx, now, &JudgeProviderZeroCarryArgs{Cursor: cursor}, model.DefaultProviderZeroCarrySettings())
		if !result.PassComplete || result.Cursor != (server.Id{}) || result.Created != 0 || result.Verdicts != 0 {
			t.Fatalf("a run over no provider reported %+v", result)
		}
		if got := testutil.ToFloat64(providerZeroCarryJudgeLastRunTimestamp); got != float64(now.Unix()) {
			t.Fatalf("the last run timestamp is %v", got)
		}
		if got := testutil.ToFloat64(providerZeroCarryJudgeLastPassTimestamp); got != float64(now.Unix()) {
			t.Fatalf("the last pass timestamp is %v", got)
		}
		if got := testutil.CollectAndCount(providerZeroCarryProviders); got != 0 {
			t.Fatalf("the providers gauge kept %d countries no verdict holds", got)
		}
		if got := testutil.ToFloat64(providerZeroCarryVerdicts); got != 0 {
			t.Fatalf("the verdicts gauge is %v", got)
		}
	})
}
