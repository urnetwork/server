// A disposable database connects real supervisor renewal recovery to an already
// claimed worker. Pool and retry barriers, not sleeps, determine every ordering.
package controller

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A returned call retains the actual store proof and its owned child deadline.
type competitionSupervisorAcquireComposedPgRenewal struct {
	call     int
	previous model.CompetitionSupervisorLease
	next     *model.CompetitionSupervisorLease
	err      error
	childErr error
	deadline time.Time
}

// A single worker retains its genuine claim and evaluation lifetime until the
// test explicitly finishes it or the real supervisor cancels its parent.
type competitionSupervisorAcquireComposedPgWorker struct {
	ctx context.Context
	job *queuedJob
	err error
}

// Only the open-round snapshot and evaluator lifetime are synthetic. Authority,
// acquisition proof, claim and renewal all use the production store in private Pg.
// Each counter has one goroutine owner and is read only after its joined result.
type competitionSupervisorAcquireComposedPgDriver struct {
	*competitionSupervisorFixture
	store        PostgresStore
	settings     *Settings
	acquired     *model.CompetitionSupervisorLease
	acquireCalls int
	renewCalls   int
	workerCalls  int
	renewals     chan competitionSupervisorAcquireComposedPgRenewal
	worker       chan competitionSupervisorAcquireComposedPgWorker
	finish       <-chan struct{}
}

// Acquisition uses the same scoped durable lease as every subsequent renewal.
func (self *competitionSupervisorAcquireComposedPgDriver) Acquire(ctx context.Context, ownerId string, ttl time.Duration) (*model.CompetitionSupervisorLease, error) {
	self.acquireCalls++
	lease, err := self.store.AcquireAutomationLease(ctx, self.settings, ownerId, ttl)
	self.acquired = lease
	return lease, err
}

// Observation is emitted only after the real renewal has completely unwound.
func (self *competitionSupervisorAcquireComposedPgDriver) Renew(ctx context.Context, lease model.CompetitionSupervisorLease, ttl time.Duration) (*model.CompetitionSupervisorLease, error) {
	self.renewCalls++
	next, err := self.store.RenewAutomationLease(ctx, self.settings, lease, ttl)
	deadline, _ := ctx.Deadline()
	self.renewals <- competitionSupervisorAcquireComposedPgRenewal{call: self.renewCalls, previous: lease, next: next, err: err, childErr: ctx.Err(), deadline: deadline}
	return next, err
}

// Claim once under actual supervisor authority, then retain the original job
// clock while the parent supervisor performs its independent renewal work.
func (self *competitionSupervisorAcquireComposedPgDriver) RunWorker(ctx context.Context, lease model.CompetitionSupervisorLease, round model.CompetitionRoundResult) error {
	self.workerCalls++
	settings := *self.settings
	settings.automationLease, settings.automationRoundId = &lease, round.RoundId
	job, err := self.store.Claim(ctx, &settings, "synthetic-composed-worker", testWorkerImageDigest())
	if err != nil || job == nil || job.StartedAt == nil {
		if err == nil {
			err = errors.New("synthetic worker did not obtain a started claim")
		}
		self.worker <- competitionSupervisorAcquireComposedPgWorker{err: err}
		return err
	}
	deadline := job.StartedAt.Add(time.Duration(settings.EvaluationPolicy.ScoreTimeoutSeconds) * time.Second)
	evaluationCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	self.worker <- competitionSupervisorAcquireComposedPgWorker{ctx: evaluationCtx, job: job}
	select {
	case <-evaluationCtx.Done():
		return evaluationCtx.Err()
	case <-self.finish:
		return evaluationCtx.Err()
	}
}

// A sole acknowledged borrower forces the first owned acquisition deadline.
// Only the genuine retry barrier releases it; the next call must renew the
// same fence while the existing worker and its persisted job clock stay live.
func TestCompetitionSupervisorAcquireRetryPgComposedKeepsOriginalWorker(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		watchdog, stopWatchdog := context.WithTimeout(t.Context(), 30*time.Second)
		defer stopWatchdog()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fixture := newCompetitionAutomationFixture(t)
		clock := &competitionLeaseRetryClock{now: fixture.now}
		fixture.store.now = clock.Now
		fixture.store.dequeueCompetitionSignal = func(context.Context, *Settings) (*server.Id, error) { return nil, nil }
		jobId := queueCompetitionAutomationPollJob(t, fixture)
		base := newCompetitionSupervisorFixture()
		base.snapshot.Seal = nil
		base.snapshot.Round = &model.CompetitionRoundResult{RoundId: fixture.round.RoundId, Epoch: 1, Status: "open", DecisionMode: model.CompetitionDecisionModeV2, ClosesAt: fixture.round.ClosesAt}
		finish, allowRetry := make(chan struct{}), make(chan struct{})
		var finishOnce, retryOnce sync.Once
		driver := &competitionSupervisorAcquireComposedPgDriver{competitionSupervisorFixture: base, store: fixture.store, settings: fixture.settings,
			renewals: make(chan competitionSupervisorAcquireComposedPgRenewal, 3), worker: make(chan competitionSupervisorAcquireComposedPgWorker, 1), finish: finish}
		// The acknowledged wait preserves its actual original total context.
		type retryBarrier struct {
			ctx   context.Context
			delay time.Duration
		}
		backoff := make(chan retryBarrier, 1)
		settings := defaultCompetitionSupervisorRenewalSettings()
		settings.Timeout, settings.AttemptTimeout = 10*time.Second, time.Second
		settings.LockRetry.Timeout, settings.LockRetry.Interval = time.Second, 10*time.Millisecond
		settings.LockRetry.Now = clock.Now
		settings.LockRetry.Wait = func(ctx context.Context, delay time.Duration) error {
			select {
			case backoff <- retryBarrier{ctx: ctx, delay: delay}:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case <-allowRetry:
				return ctx.Err()
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		ticks, accepted := make(chan time.Time, 1), make(chan struct{}, 1)
		supervisor := &CompetitionSupervisor{Enabled: true, OwnerId: "synthetic-composed-supervisor", Driver: driver, LeaseDuration: time.Minute,
			Now: clock.Now, leaseRetrySettings: &settings, renewTickForTest: ticks, renewedForTest: func() { accepted <- struct{}{} }}
		// Receiving this result joins both the worker and renewal owner.
		type reconcileResult struct {
			state string
			err   error
		}
		result, joined := make(chan reconcileResult, 1), make(chan struct{})
		releasePool, restorePool := func() {}, func() {}
		defer func() {
			cancel()
			retryOnce.Do(func() { close(allowRetry) })
			finishOnce.Do(func() { close(finish) })
			releasePool()
			select {
			case <-joined:
			case <-watchdog.Done():
				t.Error("composed supervisor did not join its canceled test lifetime")
			}
			restorePool()
		}()
		go func() {
			state, err := supervisor.Reconcile(ctx)
			result <- reconcileResult{state: state, err: err}
			close(joined)
		}()
		var worker competitionSupervisorAcquireComposedPgWorker
		select {
		case worker = <-driver.worker:
			if worker.err != nil || worker.job.JobId != jobId {
				t.Fatal("real supervised claim failed", worker.err)
			}
		case early := <-result:
			t.Fatal("supervisor ended before its worker claim", early)
		case <-watchdog.Done():
			t.Fatal("supervisor did not reach its claimed-worker barrier")
		}
		originalLease := *driver.acquired
		originalJob := readCompetitionLeasePgSnapshot(watchdog, jobId)
		originalDeadline := worker.job.StartedAt.Add(time.Duration(fixture.settings.EvaluationPolicy.ScoreTimeoutSeconds) * time.Second)
		if deadline, ok := worker.ctx.Deadline(); !ok || !deadline.Equal(originalDeadline) || worker.ctx.Err() != nil || originalJob.attempts != 1 {
			t.Fatal("claimed worker did not retain its original live execution budget")
		}
		restorePool = competitionSupervisorAcquirePgPool(t)
		releasePool = holdCompetitionSupervisorAcquirePgPool(t, watchdog)
		ticks <- clock.Now()
		var barrier retryBarrier
		select {
		case barrier = <-backoff:
		case early := <-result:
			t.Fatal("real acquire deadline canceled the worker instead of reaching recovery", early)
		case <-watchdog.Done():
			t.Fatal("real acquire deadline did not reach its retry barrier")
		}
		first := <-driver.renewals
		proof, ok := first.err.(competitionSupervisorRenewError)
		if first.call != 1 || first.next != nil || first.childErr != context.DeadlineExceeded || !errors.Is(first.err, context.DeadlineExceeded) ||
			!ok || !competitionSupervisorUnadmittedDeadline(first.err) || proof.acquire.PoolEntry.Acquired != 1 || proof.acquire.PoolEntry.Maximum != 1 || proof.read.AcquireSucceeded != 0 || proof.callbackEntered {
			t.Fatal("composed renewal did not return genuine wholly unadmitted deadline proof", first.err)
		}
		totalDeadline, hasTotalDeadline := barrier.ctx.Deadline()
		if !hasTotalDeadline || totalDeadline.Sub(first.deadline) != settings.Timeout-settings.LockRetry.Timeout || barrier.delay != settings.LockRetry.Interval ||
			barrier.ctx.Err() != nil || worker.ctx.Err() != nil || ctx.Err() != nil {
			t.Fatal("recovery reset its total allowance or canceled the independent worker")
		}
		releasePool()
		if unchanged := readCompetitionLeasePgSnapshot(watchdog, jobId); !reflect.DeepEqual(unchanged, originalJob) {
			t.Fatal("unadmitted renewal changed the original claimed job")
		}
		readLease := func() (lease model.CompetitionSupervisorLease) {
			server.Db(watchdog, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(watchdog, `SELECT owner_id,fencing_token,lease_expires_at FROM competition_supervisor_lease
				 WHERE environment=$1 AND competition_id=$2 AND season_id=$3`, originalLease.Environment, originalLease.CompetitionId, originalLease.SeasonId).Scan(
					&lease.OwnerId, &lease.FencingToken, &lease.LeaseExpiresAt))
			})
			return lease
		}
		if unchanged := readLease(); unchanged.OwnerId != originalLease.OwnerId || unchanged.FencingToken != originalLease.FencingToken || !unchanged.LeaseExpiresAt.Equal(originalLease.LeaseExpiresAt) {
			t.Fatal("unadmitted renewal changed durable supervisor authority")
		}
		clock.Advance(settings.AttemptTimeout + barrier.delay)
		retryOnce.Do(func() { close(allowRetry) })
		select {
		case <-accepted:
		case early := <-result:
			t.Fatal("supervisor ended before accepting its recovered renewal", early)
		case <-watchdog.Done():
			t.Fatal("released pool did not restore the original fenced supervisor")
		}
		second := <-driver.renewals
		if second.call != 2 || second.err != nil || second.next == nil || second.childErr != nil || !second.deadline.After(first.deadline) || second.deadline.After(totalDeadline) ||
			second.previous.OwnerId != originalLease.OwnerId || second.previous.FencingToken != originalLease.FencingToken || !second.previous.LeaseExpiresAt.Equal(originalLease.LeaseExpiresAt) ||
			second.next.OwnerId != originalLease.OwnerId || second.next.FencingToken != originalLease.FencingToken || !second.next.LeaseExpiresAt.After(originalLease.LeaseExpiresAt) {
			t.Fatal("recovery did not renew the same confirmed fence within its original total budget", second.err)
		}
		if current := readLease(); current.OwnerId != originalLease.OwnerId || current.FencingToken != originalLease.FencingToken || !current.LeaseExpiresAt.Equal(second.next.LeaseExpiresAt) {
			t.Fatal("recovered renewal did not commit the acknowledged original fence")
		}
		if after := readCompetitionLeasePgSnapshot(watchdog, jobId); !reflect.DeepEqual(after, originalJob) {
			t.Fatal("supervisor recovery restarted, handed back or reset the existing claimed job")
		}
		if deadline, ok := worker.ctx.Deadline(); !ok || !deadline.Equal(originalDeadline) || worker.ctx.Err() != nil || ctx.Err() != nil {
			t.Fatal("supervisor recovery reset or canceled the original worker lifetime")
		}
		finishOnce.Do(func() { close(finish) })
		select {
		case completed := <-result:
			if completed.err != nil || completed.state != "evaluating" || driver.acquireCalls != 1 || driver.renewCalls != 2 || driver.workerCalls != 1 {
				t.Fatal("recovered supervisor did not preserve one joined worker and one acquired fence", completed)
			}
		case <-watchdog.Done():
			t.Fatal("recovered supervisor did not join normal worker completion")
		}
	})
}
