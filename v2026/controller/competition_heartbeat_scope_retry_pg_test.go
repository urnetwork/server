// Genuine distributed custody and one owned advisory holder prove the exact
// three returned rollbacks and a fourth successful heartbeat without a new run.
package controller

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestCompetitionHeartbeatScopeRetryPgFourthAttemptPreservesDistributedClaim(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(tb.Context(), 45*time.Second)
		defer cancel()
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		selected := fixture.selected(0)
		const worker = "synthetic-scope-retry-worker"
		job, err := fixture.store.Claim(ctx, selected, worker, testWorkerImageDigest())
		if err != nil || job == nil {
			tb.Fatal("genuine distributed claim failed", err)
		}
		authority := withCompetitionJobAuthority(ctx, job)
		before := readCompetitionDistributedMergedHeartbeat(ctx, job.JobId)
		fixture.clock.Advance(time.Second)
		held, release := make(chan struct{}), make(chan struct{})
		holderDone := make(chan error, 1)
		var once sync.Once
		unlock := func() { once.Do(func() { close(release) }) }
		go func() {
			holderDone <- captureDatabaseError(func() {
				server.MaintenanceTx(ctx, func(tx server.PgTx) {
					automationScopeLockInTx(ctx, tx, selected)
					close(held)
					select {
					case <-release:
					case <-ctx.Done():
						panic(ctx.Err())
					}
				}, server.OptNoRetry())
			})
		}()
		joined := false
		defer func() {
			unlock()
			if !joined {
				select {
				case <-holderDone:
				case <-ctx.Done():
					tb.Error("owned advisory holder did not join")
				}
			}
		}()
		select {
		case <-held:
		case err := <-holderDone:
			joined = true
			tb.Fatal("owned advisory holder failed", err)
		case <-ctx.Done():
			tb.Fatal("owned scope lock was not acknowledged")
		}
		// The disabled extension is the real old-code control, not a fake error.
		for _, historical := range []bool{true, false} {
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = fixture.clock.Now
			if historical {
				settings.ScopeLockMaximumAttempts = 0
			}
			originalWait := settings.LeaseRetry.Wait
			calls, failures := 0, 0
			settings.LeaseRetry.Wait = func(wait context.Context, delay time.Duration) error {
				if !historical && failures == 3 {
					if after := readCompetitionDistributedMergedHeartbeat(ctx, job.JobId); !reflect.DeepEqual(before, after) {
						tb.Error("failed advisory statements mutated committed custody")
					}
					unlock()
					select {
					case err := <-holderDone:
						joined = true
						if err != nil {
							return err
						}
					case <-wait.Done():
						return wait.Err()
					}
				}
				return originalWait(wait, delay)
			}
			ack, renewErr := retryCompetitionJobHeartbeatOperation(authority, *job.LeaseExpiresAt, fixture.clock.Now().Add(time.Minute), settings, func(attempt context.Context) error {
				calls++
				err := fixture.store.Heartbeat(attempt, selected, worker, job.JobId)
				if err != nil {
					proof, ok := err.(competitionHeartbeatDatabaseError)
					if !ok || !competitionHeartbeatScopeLockTimeout(err) || proof.snapshot.SqlShape != competitionHeartbeatDistributedSlotShape || proof.snapshot.Transaction.CommitAttempts != 0 || proof.snapshot.Transaction.RollbackAcknowledged != 1 || proof.snapshot.Transaction.ReleaseReturned != 1 || proof.snapshot.Transaction.LastRelease.Disposition != server.DbTxReleaseIdle {
						tb.Error("real first scope lock did not return exact closed source proof", competitionHeartbeatFailureDetails(err))
						return errors.New("synthetic test proof validation failed")
					}
					failures++
				}
				return err
			})
			if historical {
				if renewErr == nil || calls != 3 || failures != 3 || !ack.IsZero() {
					tb.Fatal("real old three-call cap did not reproduce premature refusal", calls, failures, renewErr)
				}
				if after := readCompetitionDistributedMergedHeartbeat(ctx, job.JobId); !reflect.DeepEqual(before, after) {
					tb.Fatal("failed old heartbeat changed original authority")
				}
			} else if renewErr != nil || calls != 4 || failures != 3 || ack.IsZero() {
				tb.Fatal("proved fourth heartbeat failed after exact holder release", calls, failures, renewErr)
			}
		}
		after := readCompetitionDistributedMergedHeartbeat(ctx, job.JobId)
		if !after.jobLease.After(before.jobLease) || !after.jobLease.Equal(after.slotLease) || !after.startedAt.Equal(before.startedAt) || !after.deadline.Equal(before.deadline) || after.fence != before.fence || after.attempt != before.attempt {
			tb.Fatal("fourth heartbeat changed the original execution clock/fence/attempt", before, after)
		}
	})
}
