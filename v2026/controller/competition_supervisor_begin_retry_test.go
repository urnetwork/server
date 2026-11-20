// Virtual owner deadlines reproduce a late BEGIN acknowledgment without a
// callback. Refusal controls retain every original authority and cleanup gate.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A real wire regression below separately authenticates this no-callback shape.
func competitionSupervisorBeginFailure() competitionSupervisorRenewError {
	live := server.DbContextBudget{State: "live", HasDeadline: true, RemainingNs: int64(time.Second)}
	stopped := server.DbContextBudget{State: "deadline", HasDeadline: true, RemainingNs: -1}
	read := server.DbReadObservationSnapshot{Finished: true, Phase: server.DbReadError, AcquireSucceeded: 1}
	read.PhaseCounts[server.DbReadAcquireBegin], read.PhaseCounts[server.DbReadAcquireDone] = 1, 1
	return competitionSupervisorRenewError{
		cause: competitionDatabasePhaseError{cause: errors.Join(server.DbContextDoneError, context.DeadlineExceeded), phase: server.DbOperationBegin},
		phase: server.DbOperationBegin, entered: live, returned: stopped, read: read,
		readPrefix: competitionSupervisorReadPrefixSnapshot{valid: true},
		acquire:    server.DbAcquireObservation{Attempt: 1, Admitted: true, Returned: true, BeforeAcquire: live, AtReturn: live},
		transaction: server.DbTxObservationSnapshot{AcquireAttempts: 1, AcquireSucceeded: 1, BeginAttempts: 1, BeginSucceeded: 1,
			RollbackAttempts: 1, RollbackAcknowledged: 1, ReleaseAttempts: 1, ReleaseReturned: 1,
			LastBegin:    server.DbTxBoundaryObservation{Before: live, AtReturn: stopped, Returned: true},
			LastRollback: server.DbTxBoundaryObservation{Before: live, AtReturn: live, Returned: true},
			LastRelease:  server.DbTxReleaseObservation{Disposition: server.DbTxReleaseIdle}},
	}
}

// Removing only the new worker predicate cancels this actual evaluation owner
// on its first read, although the original lease and total budget remain live.
func TestCompetitionSupervisorWorkerBeginRetryPreservesEvaluationOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		settings := defaultCompetitionSupervisorWorkerFenceSettings()
		ticks, accepted := make(chan time.Time), make(chan struct{}, 1)
		settings.tickForTest, settings.confirmedForTest = ticks, func() { accepted <- struct{}{} }
		started := time.Now()
		originalDeadline := started.Add(3 * time.Hour)
		readCalls, runCalls := 0, 0
		workerContext := make(chan context.Context, 1)
		done := make(chan error, 1)
		go func() {
			done <- runCompetitionSupervisorWorkerFenced(ctx, started.Add(time.Minute), settings,
				func(attempt context.Context) (time.Time, error) {
					readCalls++
					if readCalls == 1 {
						<-attempt.Done()
						return time.Time{}, competitionSupervisorBeginFailure()
					}
					return time.Now().Add(time.Minute), nil
				}, func(parent context.Context) error {
					runCalls++
					evaluation, stop := context.WithDeadline(parent, originalDeadline)
					defer stop()
					workerContext <- evaluation
					<-evaluation.Done()
					return evaluation.Err()
				})
		}()
		evaluation := <-workerContext
		ticks <- started
		select {
		case <-accepted:
		case <-evaluation.Done():
			t.Fatal("closed late-BEGIN read canceled the still-authorized original evaluation")
		}
		if deadline, present := evaluation.Deadline(); !present || !deadline.Equal(originalDeadline) || readCalls != 2 || runCalls != 1 ||
			time.Since(started) != settings.Retry.AttemptTimeout+settings.Retry.LockRetry.Interval {
			t.Fatal("BEGIN recovery reset an evaluation, owner or retry clock", readCalls, runCalls, time.Since(started))
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("recovered worker did not join its original canceled owner", err)
		}
	})
}

// A late successful BEGIN is distinct from setup, an unconfirmed BEGIN and
// any history with business work, a commit or unacknowledged cleanup.
func TestCompetitionSupervisorWorkerBeginRetryRejectsIncompleteHistory(t *testing.T) {
	if !competitionSupervisorWorkerBeginDeadline(competitionSupervisorBeginFailure()) {
		t.Fatal("complete owned late-BEGIN rollback was refused")
	}
	for _, check := range []struct {
		name   string
		mutate func(*competitionSupervisorRenewError)
	}{
		{name: "session setup", mutate: func(p *competitionSupervisorRenewError) { p.phase = server.DbOperationSessionSetup }},
		{name: "unknown phase", mutate: func(p *competitionSupervisorRenewError) { p.phase = server.DbOperationUnknown }},
		{name: "admission phase", mutate: func(p *competitionSupervisorRenewError) { p.phase = server.DbOperationAdmission }},
		{name: "callback phase", mutate: func(p *competitionSupervisorRenewError) { p.phase = server.DbOperationCallback }},
		{name: "callback entered", mutate: func(p *competitionSupervisorRenewError) { p.callbackEntered = true }},
		{name: "earlier callback", mutate: func(p *competitionSupervisorRenewError) { p.callbacks = 1 }},
		{name: "transaction callback", mutate: func(p *competitionSupervisorRenewError) { p.transaction.CallbackEntered = 1 }},
		{name: "business step", mutate: func(p *competitionSupervisorRenewError) { p.step = competitionSupervisorWorkerFenceRead }},
		{name: "prefix used", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.count = 1 }},
		{name: "prefix bypass", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.bypasses = 1 }},
		{name: "prefix invalid", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.valid = false }},
		{name: "read unfinished", mutate: func(p *competitionSupervisorRenewError) { p.read.Finished = false }},
		{name: "earlier read admission", mutate: func(p *competitionSupervisorRenewError) { p.read.AcquireSucceeded = 2 }},
		{name: "read query", mutate: func(p *competitionSupervisorRenewError) { p.read.PhaseCounts[server.DbReadQueryBegin] = 1 }},
		{name: "read row", mutate: func(p *competitionSupervisorRenewError) { p.read.Rows = 1 }},
		{name: "acquire unknown", mutate: func(p *competitionSupervisorRenewError) { p.acquire.Returned = false }},
		{name: "acquire refused", mutate: func(p *competitionSupervisorRenewError) { p.acquire.Admitted = false }},
		{name: "earlier admitted acquisition", mutate: func(p *competitionSupervisorRenewError) { p.transaction.AcquireSucceeded = 2 }},
		{name: "BEGIN unknown", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastBegin.Returned = false }},
		{name: "BEGIN failed", mutate: func(p *competitionSupervisorRenewError) { p.transaction.BeginSucceeded = 0 }},
		{name: "second BEGIN", mutate: func(p *competitionSupervisorRenewError) { p.transaction.BeginAttempts = 2 }},
		{name: "BEGIN returned live", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastBegin.AtReturn.State = "live" }},
		{name: "BEGIN returned canceled", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastBegin.AtReturn.State = "canceled" }},
		{name: "BEGIN unowned", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastBegin.Before.HasDeadline = false }},
		{name: "commit attempted", mutate: func(p *competitionSupervisorRenewError) { p.transaction.CommitAttempts = 1 }},
		{name: "commit acknowledged", mutate: func(p *competitionSupervisorRenewError) { p.transaction.CommitAcknowledged = 1 }},
		{name: "commit boundary", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastCommit.Returned = true }},
		{name: "rollback pending", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastRollback.Returned = false }},
		{name: "rollback unknown", mutate: func(p *competitionSupervisorRenewError) { p.transaction.RollbackAcknowledged = 0 }},
		{name: "rollback stopped", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastRollback.AtReturn.State = "deadline" }},
		{name: "release pending", mutate: func(p *competitionSupervisorRenewError) { p.transaction.ReleaseReturned = 0 }},
		{name: "release unknown", mutate: func(p *competitionSupervisorRenewError) {
			p.transaction.LastRelease.Disposition = server.DbTxReleaseUnknown
		}},
		{name: "destruction transferred", mutate: func(p *competitionSupervisorRenewError) {
			p.transaction.LastRelease.Disposition = server.DbTxReleaseDestroyTransferred
		}},
		{name: "caller canceled", mutate: func(p *competitionSupervisorRenewError) { p.returned.State = "canceled" }},
		{name: "caller still live", mutate: func(p *competitionSupervisorRenewError) { p.returned.State = "live" }},
	} {
		proof := competitionSupervisorBeginFailure()
		check.mutate(&proof)
		if competitionSupervisorWorkerBeginDeadline(proof) {
			t.Errorf("%s granted BEGIN continuation", check.name)
		}
	}
}

// A deadline leaf cannot erase independent cancellation, ownership loss, SQL,
// network, unknown wrapper or unrelated outer-branch meaning.
func TestCompetitionSupervisorWorkerBeginRetryRejectsMixedCauses(t *testing.T) {
	for _, cause := range []error{context.Canceled, ErrLeaseLost, errors.New("synthetic refusal"),
		errors.Join(context.DeadlineExceeded, context.Canceled), errors.Join(context.DeadlineExceeded, ErrLeaseLost),
		&net.OpError{Op: "read", Net: "synthetic", Err: context.DeadlineExceeded},
		competitionSupervisorClaimedRetryDeadline{cause: context.DeadlineExceeded},
		server.NewErrorCauseBatch([]error{context.DeadlineExceeded, nil}),
	} {
		proof := competitionSupervisorBeginFailure()
		proof.cause = competitionDatabasePhaseError{cause: cause, phase: server.DbOperationBegin}
		if competitionSupervisorWorkerBeginDeadline(proof) {
			t.Error("mixed or untrusted cause granted BEGIN continuation", cause)
		}
	}
	proof := competitionSupervisorBeginFailure()
	for _, cause := range []error{fmt.Errorf("synthetic outer: %w", proof), errors.Join(proof, context.DeadlineExceeded), &proof} {
		if competitionSupervisorWorkerBeginDeadline(cause) {
			t.Error("non-root proof granted BEGIN continuation")
		}
	}
}

// The same classified proof may never dispatch another read after parent
// cancellation or confirmed expiry, even with the owned retry reserve unused.
func TestCompetitionSupervisorWorkerBeginRetryPreservesExpiryAndParent(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			settings := defaultCompetitionSupervisorWorkerFenceSettings()
			ticks := make(chan time.Time)
			settings.tickForTest = ticks
			started := time.Now()
			expires := started.Add(settings.Retry.AttemptTimeout)
			if cancelParent {
				expires = started.Add(time.Minute)
			}
			calls := 0
			entered, done := make(chan struct{}), make(chan error, 1)
			go func() {
				done <- runCompetitionSupervisorWorkerFenced(ctx, expires, settings,
					func(attempt context.Context) (time.Time, error) {
						calls++
						<-attempt.Done()
						if cancelParent {
							cancel()
						}
						return time.Time{}, competitionSupervisorBeginFailure()
					}, func(parent context.Context) error { close(entered); <-parent.Done(); return parent.Err() })
			}()
			<-entered
			ticks <- started
			err := <-done
			if calls != 1 || cancelParent && !errors.Is(err, context.Canceled) || !cancelParent && !errors.Is(err, ErrLeaseLost) {
				t.Fatal("classified BEGIN proof revived a canceled/expired owner", cancelParent, calls, err)
			}
		})
	}
}

// Each completed failure spends the original total bound; repeated positive
// proof cannot manufacture a fresh allowance or keep a failed worker alive.
func TestCompetitionSupervisorWorkerBeginRetryKeepsTotalBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorWorkerFenceSettings()
		ticks := make(chan time.Time)
		settings.tickForTest = ticks
		settings.Retry.LockRetry.MaximumAttempts = 8
		started, calls := time.Now(), 0
		entered, done := make(chan struct{}), make(chan error, 1)
		go func() {
			done <- runCompetitionSupervisorWorkerFenced(t.Context(), started.Add(time.Hour), settings,
				func(attempt context.Context) (time.Time, error) {
					calls++
					<-attempt.Done()
					return time.Time{}, competitionSupervisorBeginFailure()
				},
				func(parent context.Context) error { close(entered); <-parent.Done(); return parent.Err() })
		}()
		<-entered
		ticks <- started
		if err := <-done; !errors.Is(err, context.DeadlineExceeded) || calls != 3 || time.Since(started) != settings.Retry.Timeout {
			t.Fatal("BEGIN recovery replaced its original cumulative owner budget", calls, time.Since(started), err)
		}
	})
}

// A shorter parent already caps the effective evaluation deadline at creation.
// Recovery preserves that cap AND the separately declared three-hour job clock;
// successful authority reads cannot extend either into a new owner lifetime.
func TestCompetitionSupervisorWorkerBeginRetryPreservesShorterParentDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		parentDeadline, parentHasDeadline := ctx.Deadline()
		settings := defaultCompetitionSupervisorWorkerFenceSettings()
		ticks, accepted := make(chan time.Time), make(chan struct{}, 1)
		settings.tickForTest, settings.confirmedForTest = ticks, func() { accepted <- struct{}{} }
		started := time.Now()
		jobDeadline := started.Add(3 * time.Hour)
		readCalls, runCalls := 0, 0
		evaluations := make(chan context.Context, 1)
		done := make(chan error, 1)
		go func() {
			done <- runCompetitionSupervisorWorkerFenced(ctx, started.Add(2*time.Minute), settings,
				func(attempt context.Context) (time.Time, error) {
					readCalls++
					if readCalls == 1 {
						<-attempt.Done()
						return time.Time{}, competitionSupervisorBeginFailure()
					}
					return started.Add(2 * time.Minute), nil
				}, func(parent context.Context) error {
					runCalls++
					evaluation, stop := context.WithDeadline(parent, jobDeadline)
					defer stop()
					evaluations <- evaluation
					<-evaluation.Done()
					return evaluation.Err()
				})
		}()
		joined := false
		defer func() {
			cancel()
			if !joined {
				<-done
			}
		}()
		var evaluation context.Context
		select {
		case evaluation = <-evaluations:
		case early := <-done:
			joined = true
			t.Fatal("shorter-parent worker refused before evaluation entry", early)
		case <-ctx.Done():
			t.Fatal("shorter-parent evaluation never entered its bounded owner", ctx.Err())
		}
		initialDeadline, present := evaluation.Deadline()
		if !parentHasDeadline || !present || !initialDeadline.Equal(parentDeadline) || !initialDeadline.Before(jobDeadline) || evaluation.Err() != nil {
			t.Fatal("short parent did not cap the initial effective evaluation clock", initialDeadline, parentDeadline, jobDeadline, evaluation.Err())
		}
		select {
		case ticks <- started:
		case early := <-done:
			joined = true
			t.Fatal("shorter-parent worker ended before its fence tick", early)
		case <-ctx.Done():
			t.Fatal("shorter-parent worker did not accept its fence tick", ctx.Err())
		}
		select {
		case <-accepted:
		case <-evaluation.Done():
			t.Fatal("closed BEGIN recovery canceled the shorter-parent evaluation early")
		}
		if after, present := evaluation.Deadline(); !present || !after.Equal(initialDeadline) || evaluation.Err() != nil ||
			jobDeadline.Sub(started) != 3*time.Hour || readCalls != 2 || runCalls != 1 {
			t.Fatal("BEGIN retry changed the initial effective or declared job deadline", after, initialDeadline, jobDeadline, evaluation.Err())
		}
		// Only the unchanged shorter parent's virtual deadline retires this
		// owner. The later supervisor/job clocks cannot keep it alive.
		err := <-done
		joined = true
		if !errors.Is(err, context.DeadlineExceeded) || evaluation.Err() != context.DeadlineExceeded ||
			time.Since(started) != time.Minute || readCalls != 2 || runCalls != 1 {
			t.Fatal("recovered evaluation outlived or shortened its original parent", time.Since(started), readCalls, runCalls, err)
		}
	})
}
