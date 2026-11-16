// Exact virtual boundaries prove a second call keeps the original confirmed
// authority rather than gaining a fresh per-call or total lifetime.
package controller

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// Eight seconds plus five seconds of first-call cleanup leaves only 1.75s
// after the inherited backoff when original host authority ends at fifteen.
func TestCompetitionHeartbeatJobLockContinuationTruncatesSecondCallAtConfirmedExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Now = time.Now
		calls := 0
		expires := started.Add(15 * time.Second)
		ack, err := retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), expires, settings, func(attempt context.Context) error {
			calls++
			if calls == 1 {
				<-attempt.Done()
				time.Sleep(5 * time.Second)
				return competitionHeartbeatJobLockFailure(attempt.Err())
			}
			if calls != 2 || time.Since(started) != 13*time.Second+settings.LeaseRetry.Interval {
				t.Fatal("prefix owner changed predecessor cleanup/backoff or dispatched a third call", calls, time.Since(started))
			}
			deadline, ok := attempt.Deadline()
			if !ok || !deadline.Equal(expires) || time.Until(deadline) >= settings.AcquireRetry.AttemptTimeout {
				t.Fatal("second call gained a fresh eight/twenty-second lifetime", deadline)
			}
			<-attempt.Done()
			return competitionHeartbeatJobLockFailure(attempt.Err())
		})
		if err == nil || !errors.Is(err, ErrLeaseLost) || calls != 2 || !ack.IsZero() || time.Since(started) != 15*time.Second {
			t.Fatal("expired original host authority allowed another call or acknowledgement", calls, ack, err, time.Since(started))
		}
	})
}
