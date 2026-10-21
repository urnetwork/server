// The completion clock belongs to the duration observation, even when the
// public adapter has no host-scope wrapper to apply parent precedence.
package monitor

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The source finishes while its parent is live; the real completion-clock
// callback cancels immediately before parsing. Both public paths must stop.
func TestPgFreezeCompletionCancellationPreventsVerdict(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, scoped := range []bool{false, true} {
		for _, outcome := range []string{"positive", "quiet", "malformed"} {
			payload := pgFreezeTestPayload(t, now, pgFreezeTestCluster(now.Add(-time.Minute), 250, 3000000000))
			if outcome == "quiet" {
				payload = pgFreezeTestPayload(t, now, nil)
			}
			if outcome == "malformed" {
				payload = "private-source-output-canary"
			}
			ctx, cancel := context.WithCancel(context.Background())
			calls, completionClocks := 0, 0
			settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				calls++
				if ctx.Err() != nil {
					t.Fatal("completion fixture canceled before the source returned")
				}
				return payload, nil
			}})
			if scoped {
				settings.ExcludedHosts = []string{"redis-1"}
			}
			settings.Now = func() time.Time {
				if calls > 0 {
					completionClocks++
					cancel()
				}
				return now
			}
			alerts, err := pgFreezeSignalForTest(t).Run(ctx, settings)
			cancel()
			if calls != 1 || completionClocks == 0 {
				t.Fatal("completion fixture did not reach the owned clock boundary")
			}
			if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
				t.Fatalf("duration completion cancellation published a verdict: scoped=%v outcome=%s", scoped, outcome)
			}
		}
	}
}
