package model

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A hot edit of the earning boundary never reinterprets retained usage: the
// subnet's usage read refuses either way the cutoff moves, and the restored
// boundary reads the usage past the cutoff as before. The payout planner and
// sender that refused the same way ended with USDC provider payouts (owner
// decisions 2026-11-12 and 2026-11-13).
func TestProviderBoundaryDriftCannotReassignSubnetUsage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		original := server.Config.RequireSimpleResource("sn.yml").Bytes()
		f := newPayoutTransitionCohort(t, ctx)
		before, after := payoutTestCutoff.Add(-time.Microsecond), payoutTestCutoff.Add(time.Microsecond)
		f.insertMinted(t, ctx, before.Add(-time.Hour), &before, after.Add(time.Hour), 11, UsdToNanoCents(1))
		f.insertMinted(t, ctx, before.Add(-time.Hour), &after, after.Add(time.Hour), 22, UsdToNanoCents(2))
		for _, changedCutoff := range []time.Time{payoutTestCutoff.Add(time.Hour), payoutTestCutoff.Add(-time.Hour)} {
			changed := bytes.Replace(original, []byte(payoutTestCutoff.Format(time.RFC3339)), []byte(changedCutoff.Format(time.RFC3339)), 1)
			release := server.Config.PushSimpleResource("sn.yml", changed)
			_, usageErr := GetStEpochProviderUsageAtEpoch(ctx, 7, before.Add(-time.Hour), after.Add(time.Hour))
			release()
			if !errors.Is(usageErr, server.ErrProviderEarningBoundaryMismatch) {
				t.Fatal("hot edit reinterpreted retained economic usage", changedCutoff, usageErr)
			}
		}
		usage, err := GetStEpochProviderUsageAtEpoch(ctx, 7, before.Add(-time.Hour), after.Add(time.Hour))
		if err != nil || len(usage) != 1 || usage[0] == nil || usage[0].PayoutByteCount != 22 {
			t.Fatal("restored policy lost retained subnet usage", usage, err)
		}
	})
}
