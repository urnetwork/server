// A worker observes its live parent's fence without renewing it. Independent
// expiry cancels work even while a read or detached cleanup remains blocked.
package controller

import (
	"context"
	"errors"
	"time"
)

// One worker owns cadence and retry budgets. Tests supply exact tick edges;
// neither observations nor retries can extend the parent's database expiry.
type competitionSupervisorWorkerFenceSettings struct {
	// Positive check cadence; nonpositive refuses to start the worker.
	Interval time.Duration
	// Owned liveness allowances never replace the confirmed expiry window.
	Retry competitionSupervisorRenewalSettings
	// Nil maps database expiry with the untruncated monotonic clock.
	authorityNow     func() time.Time
	tickForTest      <-chan time.Time
	confirmedForTest func()
}

// Preserve the five-second check cadence and shared liveness retry defaults.
func defaultCompetitionSupervisorWorkerFenceSettings() competitionSupervisorWorkerFenceSettings {
	return competitionSupervisorWorkerFenceSettings{Interval: 5 * time.Second, Retry: defaultCompetitionSupervisorRenewalSettings()}
}

// The run callback owns/join its own worker tree; this wrapper additionally
// joins the fence reader before returning its original finite cancellation cause.
func runCompetitionSupervisorWorkerFenced(ctx context.Context, expiresAt time.Time, settings competitionSupervisorWorkerFenceSettings, read func(context.Context) (time.Time, error), run func(context.Context) error) error {
	if settings.Interval <= 0 {
		return errors.New("supervisor worker fence interval must be positive")
	}
	authority, err := newCompetitionConfirmedAuthority(ctx, expiresAt, settings.authorityNow)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	failureObservation, _ := ctx.Value(competitionWorkerFailureObservationKey{}).(*CompetitionWorkerFailureObservation)
	var failedFenceError error
	var failedFenceOwner *competitionHeartbeatOwnerObservation
	go func() {
		defer close(done)
		currentExpiry := expiresAt
		for {
			tick := settings.tickForTest
			if tick == nil {
				tick = time.After(settings.Interval)
			}
			select {
			case <-authority.ctx.Done():
				return
			case <-tick:
			}
			var nextExpiry time.Time
			var observation *competitionHeartbeatOwnerObservation
			if failureObservation != nil {
				observation = newCompetitionHeartbeatOwnerObservation(authority.ctx, currentExpiry, time.Time{})
			}
			err := retryCompetitionAuthorityRenewalObserved(authority.ctx, currentExpiry, settings.Retry, func(attempt context.Context) error {
				observation.beginStore(attempt)
				var err error
				nextExpiry, err = read(attempt)
				observation.finishStore(attempt, err == nil, nil)
				return err
			}, func(err error) bool {
				return competitionSupervisorUnadmittedDeadline(err) || competitionSupervisorWorkerFenceDeadline(err)
			}, observation)
			if err == nil {
				err = authority.confirm(nextExpiry)
			}
			if err != nil {
				failedFenceError, failedFenceOwner = err, observation
				authority.cancel(err)
				return
			}
			currentExpiry = nextExpiry
			if settings.confirmedForTest != nil {
				settings.confirmedForTest()
			}
		}
	}()
	err = run(authority.ctx)
	cause := context.Cause(authority.ctx)
	authority.close()
	<-done
	if cause != nil {
		return observeCompetitionWorkerFenceReturn(failureObservation, cause, failedFenceError, failedFenceOwner)
	}
	if finalCause := context.Cause(authority.ctx); finalCause != nil && finalCause != context.Canceled {
		return observeCompetitionWorkerFenceReturn(failureObservation, finalCause, failedFenceError, failedFenceOwner)
	}
	return err
}
