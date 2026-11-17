// Synthetic fence ticks, virtual time and explicit joins exercise the real
// retry owner and report projection without a database, process or evaluator.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// A successful read returned after its child deadline is still refused by the
// existing owner. Both children join before failure-only facts are published.
func TestCompetitionWorkerFenceOwnerObservesBareLateSuccessDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(t.Context())
		defer cancel()
		ctx, observation := ObserveCompetitionWorkerFailure(parent)
		settings := defaultCompetitionSupervisorWorkerFenceSettings()
		ticks := make(chan time.Time)
		settings.tickForTest = ticks
		started, readCalls, runCalls := time.Now(), 0, 0
		runEntered, runCanceled, releaseRun := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(releaseRun) })
		done := make(chan error, 1)
		go func() {
			done <- runCompetitionSupervisorWorkerFenced(ctx, started.Add(time.Minute), settings,
				func(attempt context.Context) (time.Time, error) {
					readCalls++
					<-attempt.Done()
					return started.Add(2 * time.Minute), nil
				}, func(parent context.Context) error {
					runCalls++
					close(runEntered)
					<-parent.Done()
					close(runCanceled)
					<-releaseRun
					return parent.Err()
				})
		}()
		<-runEntered
		ticks <- started
		<-runCanceled
		synctest.Wait()
		if observation.workerFenceOwner != nil || time.Since(started) != settings.Retry.AttemptTimeout {
			t.Fatal("fence facts escaped before the joined return or changed the owned deadline")
		}
		releaseOnce.Do(func() { close(releaseRun) })
		err := <-done
		if err != context.DeadlineExceeded || !errors.Is(err, context.DeadlineExceeded) || readCalls != 1 || runCalls != 1 || ctx.Err() != nil {
			t.Fatal("observation changed exact cause, dispatch count or live parent", err, readCalls, runCalls)
		}
		content, marshalErr := MarshalCompetitionWorkerFailureReportObserved(err, observation)
		report, decodeErr := DecodeCompetitionWorkerFailureReport(content)
		if marshalErr != nil || decodeErr != nil || report.Category != "deadline_exceeded" || report.DatabasePhase != 0 || report.SupervisorStep != 0 || report.AcquireAttempts != 0 || report.WorkerFenceOwner == nil {
			t.Fatal("bare owner deadline lost finite facts or invented database history", marshalErr, decodeErr)
		}
		owner := report.WorkerFenceOwner
		if owner.Boundary != uint8(competitionHeartbeatOwnerAttemptAfterSuccess) || owner.StoreCalls != 1 || owner.LastStoreOutcome != uint8(competitionHeartbeatStoreSucceeded) ||
			owner.ParentAtEntry.State != "live" || owner.ParentAtReturn.State != "live" || owner.TotalAtReturn.State != "live" || owner.AttemptAtEntry.State != "live" || owner.AttemptAtReturn.State != "deadline" ||
			!owner.HasConfirmedLeaseExpiry || owner.ConfirmedLeaseExpiryUnixNs != started.Add(time.Minute).UnixNano() || len(content) > 8*1024 {
			t.Fatal("owner facts describe another return or changed confirmed authority", owner)
		}
	})
}

// The retry wait can select the same bare sentinel while its last store really
// failed. This records that distinction without changing the inherited window.
func TestCompetitionWorkerFenceOwnerObservesBareWaitDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(t.Context())
		defer cancel()
		ctx, observation := ObserveCompetitionWorkerFailure(parent)
		settings := defaultCompetitionSupervisorWorkerFenceSettings()
		ticks := make(chan time.Time)
		settings.tickForTest = ticks
		waitCalls := 0
		settings.Retry.LockRetry.Wait = func(wait context.Context, _ time.Duration) error {
			waitCalls++
			<-wait.Done()
			return wait.Err()
		}
		started, readCalls := time.Now(), 0
		runEntered := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- runCompetitionSupervisorWorkerFenced(ctx, started.Add(time.Minute), settings,
				func(context.Context) (time.Time, error) {
					readCalls++
					return time.Time{}, &pgconn.PgError{Code: "55P03", Message: "synthetic-private-lock-cause"}
				}, func(parent context.Context) error { close(runEntered); <-parent.Done(); return parent.Err() })
		}()
		<-runEntered
		ticks <- started
		err := <-done
		owner := observation.workerFenceSnapshot(err)
		if err != context.DeadlineExceeded || !errors.Is(err, context.DeadlineExceeded) || readCalls != 1 || waitCalls != 1 || time.Since(started) != settings.Retry.LockRetry.Timeout || ctx.Err() != nil || owner == nil ||
			owner.Boundary != uint8(competitionHeartbeatOwnerWaitError) || owner.LastStoreOutcome != uint8(competitionHeartbeatStoreFailed) || owner.StoreCalls != 1 || owner.ParentAtReturn.State != "live" || owner.TotalAtReturn.State != "live" {
			t.Fatal("wait deadline borrowed the store failure or altered its budget", err, readCalls, waitCalls, owner)
		}
		content, _ := MarshalCompetitionWorkerFailureReportObserved(err, observation)
		if strings.Contains(string(content), "synthetic-private-lock-cause") {
			t.Fatal("a last store cause escaped the owner projection")
		}
	})
}

// Diagnostics cannot add a fourth call or convert an unproved generic deadline
// into the authentic completed-deadline predicate's separate retry allowance.
func TestCompetitionWorkerFenceOwnerPreservesCallCapAndProofGate(t *testing.T) {
	for _, genericDeadline := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, observation := ObserveCompetitionWorkerFailure(parent)
			settings := defaultCompetitionSupervisorWorkerFenceSettings()
			ticks := make(chan time.Time)
			settings.tickForTest = ticks
			settings.Retry.LockRetry.Wait = func(context.Context, time.Duration) error { return nil }
			cause := error(&pgconn.PgError{Code: "55P03", Message: "synthetic-private-retry-cause"})
			wantCalls := settings.Retry.LockRetry.MaximumAttempts
			if genericDeadline {
				cause, wantCalls = context.DeadlineExceeded, 1
			}
			readCalls := 0
			runEntered := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- runCompetitionSupervisorWorkerFenced(ctx, time.Now().Add(time.Minute), settings,
					func(context.Context) (time.Time, error) { readCalls++; return time.Time{}, cause },
					func(parent context.Context) error { close(runEntered); <-parent.Done(); return parent.Err() })
			}()
			<-runEntered
			ticks <- time.Now()
			err := <-done
			owner := observation.workerFenceSnapshot(err)
			if err != cause || !errors.Is(err, cause) || readCalls != wantCalls || settings.Retry.LockRetry.MaximumAttempts != 3 || owner == nil || owner.StoreCalls != uint64(wantCalls) ||
				owner.Boundary != uint8(competitionHeartbeatOwnerStoreError) || owner.LastStoreOutcome != uint8(competitionHeartbeatStoreFailed) {
				t.Fatal("owner diagnostics altered exact error, call cap or genuine proof gate", genericDeadline, err, readCalls, owner)
			}
			if !genericDeadline {
				var postgres *pgconn.PgError
				if !errors.As(err, &postgres) || postgres != cause {
					t.Fatal("store error identity/type changed")
				}
			} else if competitionSupervisorUnadmittedDeadline(err) || competitionSupervisorWorkerFenceDeadline(err) {
				t.Fatal("observing a bare deadline manufactured an authentic database retry proof")
			}
		})
	}
}

// A healthy fence check cannot decorate a later unrelated run failure. The
// original evaluation clock and single worker survive that successful read.
func TestCompetitionWorkerFenceOwnerDoesNotBorrowHealthyReadForRunFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(t.Context())
		defer cancel()
		ctx, observation := ObserveCompetitionWorkerFailure(parent)
		settings := defaultCompetitionSupervisorWorkerFenceSettings()
		ticks, confirmed := make(chan time.Time), make(chan struct{}, 1)
		settings.tickForTest, settings.confirmedForTest = ticks, func() { confirmed <- struct{}{} }
		cause := errors.New("synthetic-private-unrelated-run-failure")
		started, readCalls, runCalls := time.Now(), 0, 0
		originalDeadline := started.Add(3 * time.Hour)
		runEntered, releaseRun := make(chan context.Context, 1), make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(releaseRun) })
		done := make(chan error, 1)
		go func() {
			done <- runCompetitionSupervisorWorkerFenced(ctx, started.Add(time.Minute), settings,
				func(context.Context) (time.Time, error) { readCalls++; return started.Add(2 * time.Minute), nil },
				func(parent context.Context) error {
					runCalls++
					evaluation, cancel := context.WithDeadline(parent, originalDeadline)
					defer cancel()
					runEntered <- evaluation
					<-releaseRun
					return cause
				})
		}()
		evaluation := <-runEntered
		ticks <- started
		<-confirmed
		deadline, present := evaluation.Deadline()
		if !present || !deadline.Equal(originalDeadline) || evaluation.Err() != nil || readCalls != 1 || runCalls != 1 || observation.workerFenceOwner != nil {
			t.Fatal("healthy observation changed the worker or original evaluation clock")
		}
		releaseOnce.Do(func() { close(releaseRun) })
		err := <-done
		legacy, _ := MarshalCompetitionWorkerFailureReport(err)
		observed, _ := MarshalCompetitionWorkerFailureReportObserved(err, observation)
		if err != cause || !bytes.Equal(legacy, observed) || strings.Contains(string(observed), cause.Error()) {
			t.Fatal("unrelated run failure borrowed a successful fence or changed private error bytes")
		}
	})
}

// Either owner may select the shared expiry sentinel. Explicit barriers force
// watchdog-first; an owned clock edge forces retry-first before the watchdog.
// Neither order grants enough provenance to emit a selected fence owner.
func TestCompetitionWorkerFenceOwnerDoesNotBorrowReadAfterAuthorityExpiry(t *testing.T) {
	for _, retryFirst := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, observation := ObserveCompetitionWorkerFailure(parent)
			settings := defaultCompetitionSupervisorWorkerFenceSettings()
			ticks := make(chan time.Time)
			settings.tickForTest = ticks
			started, readCalls := time.Now(), 0
			expiresAt := started.Add(time.Second)
			policyNow := started
			if retryFirst {
				settings.Retry.LockRetry.Now = func() time.Time { return policyNow }
			}
			runEntered, readEntered, runCanceled, releaseRead := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(releaseRead) })
			done := make(chan error, 1)
			go func() {
				done <- runCompetitionSupervisorWorkerFenced(ctx, expiresAt, settings,
					func(context.Context) (time.Time, error) {
						readCalls++
						close(readEntered)
						if retryFirst {
							policyNow = expiresAt
						} else {
							<-releaseRead
						}
						return started.Add(time.Hour), nil
					}, func(parent context.Context) error {
						close(runEntered)
						<-parent.Done()
						close(runCanceled)
						return parent.Err()
					})
			}()
			<-runEntered
			ticks <- started
			<-readEntered
			<-runCanceled
			if !retryFirst {
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatal("watchdog path returned before the held reader joined", err)
				default:
				}
				if observation.workerFenceOwner != nil {
					t.Fatal("watchdog emitted facts before the reader joined")
				}
				releaseOnce.Do(func() { close(releaseRead) })
			}
			err := <-done
			wantElapsed := time.Second
			if retryFirst {
				wantElapsed = 0
			}
			legacy, _ := MarshalCompetitionWorkerFailureReport(err)
			observed, _ := MarshalCompetitionWorkerFailureReportObserved(err, observation)
			if err != ErrLeaseLost || !errors.Is(err, ErrLeaseLost) || time.Since(started) != wantElapsed || readCalls != 1 || observation.workerFenceSnapshot(err) != nil || ctx.Err() != nil || !bytes.Equal(legacy, observed) {
				t.Fatal("shared expiry cause changed identity, revived authority or claimed provenance", retryFirst, err, readCalls, time.Since(started))
			}
		})
	}
}

// A legal error need not be comparable. Such roots keep the legacy report,
// rather than invoking unknown inspection methods or panicking on identity.
type competitionWorkerFenceUncomparableFailure []string

// This payload is deliberately private and must never be reflected by a report.
func (self competitionWorkerFenceUncomparableFailure) Error() string {
	return "synthetic-private-uncomparable-cause"
}

// A statically comparable wrapper can still contain an uncomparable dynamic
// value. Identity observation must not call interface equality on that graph.
type competitionWorkerFenceNestedFailure struct{ cause error }

// The nested original remains private and is never inspected for admission.
func (self competitionWorkerFenceNestedFailure) Error() string { return self.cause.Error() }

// Projection uses the exact source-bound cause, copies facts and excludes every
// arbitrary string, even if an internal invalid observation is supplied later.
func TestCompetitionWorkerFenceOwnerReportIdentityCopyAndPrivacy(t *testing.T) {
	ctx, observation := ObserveCompetitionWorkerFailure(t.Context())
	owner := newCompetitionHeartbeatOwnerObservation(ctx, time.Now().Add(time.Minute), time.Time{})
	owner.beginStore(ctx)
	owner.finishStore(ctx, false, nil)
	owner.returning(competitionHeartbeatOwnerStoreError, ctx, ctx)
	cause := &pgconn.PgError{Code: "55P03", Message: "synthetic-private-query-or-arguments"}
	if returned := observeCompetitionWorkerFenceReturn(observation, cause, cause, owner); returned != cause {
		t.Fatal("publishing wrapped or replaced the exact error")
	}
	copy := observation.workerFenceSnapshot(cause)
	copy.ParentAtReturn.State, copy.Boundary = "synthetic-private-state", 255
	content, err := MarshalCompetitionWorkerFailureReportObserved(cause, observation)
	if err != nil || strings.Contains(string(content), "synthetic-private") {
		t.Fatal("private text escaped or returned facts retained mutable pointers", err)
	}
	for _, other := range []error{fmt.Errorf("synthetic wrapper: %w", cause), errors.New(cause.Error()), competitionWorkerFenceUncomparableFailure{"synthetic"}, competitionWorkerFenceNestedFailure{cause: competitionWorkerFenceUncomparableFailure{"synthetic"}}} {
		if observation.workerFenceSnapshot(other) != nil {
			t.Fatal("another error borrowed original owner facts")
		}
	}
	uncomparable := error(competitionWorkerFenceUncomparableFailure{"synthetic"})
	returned := observeCompetitionWorkerFenceReturn(observation, uncomparable, uncomparable, owner)
	actual, ok := returned.(competitionWorkerFenceUncomparableFailure)
	original := uncomparable.(competitionWorkerFenceUncomparableFailure)
	if !ok || len(actual) != len(original) || &actual[0] != &original[0] {
		t.Fatal("uncomparable cause was replaced")
	}
	nested := competitionWorkerFenceNestedFailure{cause: uncomparable}
	returned = observeCompetitionWorkerFenceReturn(observation, nested, nested, owner)
	actualNested, ok := returned.(competitionWorkerFenceNestedFailure)
	if !ok || &actualNested.cause.(competitionWorkerFenceUncomparableFailure)[0] != &original[0] {
		t.Fatal("nested uncomparable cause was replaced")
	}
	for _, invalid := range []string{"schema", "boundary", "outcome", "count-over-cap", "count-zero-with-reply", "count-without-reply", "parent-entry", "parent-return", "total-return", "attempt-entry", "attempt-return"} {
		value := *observation.workerFenceOwner
		switch invalid {
		case "schema":
			value.Schema = 2
		case "boundary":
			value.Boundary = 255
		case "outcome":
			value.LastStoreOutcome = 255
		case "count-over-cap":
			value.StoreCalls = uint64(defaultCompetitionSupervisorWorkerFenceSettings().Retry.LockRetry.MaximumAttempts) + 1
		case "count-zero-with-reply":
			value.StoreCalls = 0
		case "count-without-reply":
			value.LastStoreOutcome = uint8(competitionHeartbeatStoreNotCalled)
		case "parent-entry":
			value.ParentAtEntry.State = "synthetic-private-state"
		case "parent-return":
			value.ParentAtReturn.State = "synthetic-private-state"
		case "total-return":
			value.TotalAtReturn.State = "synthetic-private-state"
		case "attempt-entry":
			value.AttemptAtEntry.State = "synthetic-private-state"
		case "attempt-return":
			value.AttemptAtReturn.State = "synthetic-private-state"
		}
		invalidObservation := &CompetitionWorkerFailureObservation{workerFenceCause: cause, workerFenceOwner: &value}
		content, err := MarshalCompetitionWorkerFailureReportObserved(cause, invalidObservation)
		report, decodeErr := DecodeCompetitionWorkerFailureReport(content)
		if err != nil || decodeErr != nil || report.WorkerFenceOwner != nil || strings.Contains(string(content), "synthetic-private") {
			t.Fatal("invalid internal observation escaped instead of preserving legacy diagnostics", invalid, err, decodeErr)
		}
	}
}

// The optional additive object has a closed schema just like the legacy fields;
// unknown nested keys/text and arbitrary enum values are not public diagnostics.
func TestCompetitionWorkerFenceOwnerDecoderRejectsUnknownNestedFacts(t *testing.T) {
	ctx, observation := ObserveCompetitionWorkerFailure(t.Context())
	owner := newCompetitionHeartbeatOwnerObservation(ctx, time.Now().Add(time.Minute), time.Time{})
	owner.returning(competitionHeartbeatOwnerAttemptBeforeDispatch, ctx, ctx)
	observeCompetitionWorkerFenceReturn(observation, context.DeadlineExceeded, context.DeadlineExceeded, owner)
	content, err := MarshalCompetitionWorkerFailureReportObserved(context.DeadlineExceeded, observation)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"schema", "boundary", "last_store_outcome", "store_calls", "negative-store-calls", "unknown-key", "parent_at_entry", "parent_at_return", "total_at_return", "attempt_at_entry", "attempt_at_return"} {
		var document map[string]any
		if err := json.Unmarshal(content, &document); err != nil {
			t.Fatal(err)
		}
		facts := document["worker_fence_owner"].(map[string]any)
		switch invalid {
		case "schema", "boundary", "last_store_outcome", "store_calls":
			facts[invalid] = 255
		case "negative-store-calls":
			facts["store_calls"] = -1
		case "unknown-key":
			facts["query"] = "synthetic-private-query"
		default:
			facts[invalid].(map[string]any)["state"] = "synthetic-private-state"
		}
		mutated, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeCompetitionWorkerFailureReport(mutated); err == nil {
			t.Fatal("unvalidated nested worker-fence facts accepted", invalid)
		}
	}
}
