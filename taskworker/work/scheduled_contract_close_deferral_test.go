// A close refused by a held owner wakes at a jittered point after the deferral.
package work

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The closes one held owner refused together used to wake together, exactly
// the deferral later, and refuse together again. A fixed jitter offset shows
// the wake is the deferral plus that offset, with the deadline unchanged.
func TestScheduledCloseHeldOwnerDeferralIsJittered(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		const offset = 1234 * time.Millisecond
		jitter := scheduledCloseDeferralJitter
		var spans []time.Duration
		scheduledCloseDeferralJitter = func(span time.Duration) time.Duration {
			spans = append(spans, span)
			return offset
		}
		defer func() { scheduledCloseDeferralJitter = jitter }()
		f := newScheduledCloseOwnerFixture(t.Context())
		payer := f.newPayer(t, t.Context(), "synthetic-jitter-payer")
		f.addLegacyContracts(t.Context(), payer, 1)
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		deadline := publishScheduledCloses(t, ctx)
		release := holdScheduledCloseOwner(t, t.Context(), payer)
		defer release()
		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		before := server.NowUtc()
		finished, retried, posts, err := worker.EvalTasks(1)
		after := server.NowUtc()
		if err != nil || len(finished) != 1 || len(retried)+len(posts) != 0 {
			t.Fatal("a held owner's close did not defer", finished, retried, posts, err)
		}
		results := readScheduledCloseResults(t, ctx)
		if len(results) != 1 || results[0].DeferredUntil == nil || results[0].RetryAt == nil || !results[0].RetryAt.Equal(deadline) {
			t.Fatal("the close did not defer with its deadline", results)
		}
		wake := *results[0].DeferredUntil
		low, high := before.Add(scheduledCloseOwnerBusyDeferral+offset), after.Add(scheduledCloseOwnerBusyDeferral+offset)
		if wake.Before(low) || high.Before(wake) || len(spans) != 1 || spans[0] != scheduledCloseOwnerBusyDeferral {
			t.Fatal("the deferral did not wake at the deferral plus its jitter", wake.Sub(before), spans)
		}
	})
}
