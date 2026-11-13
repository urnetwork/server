// Synthetic facts and virtual owner clocks exercise diagnostics, never new
// retry authority. No production identity, database, evaluator or token runs.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// This is unit-test metadata, not evidence that a real COMMIT executed.
func syntheticHeartbeatOwnerDatabase() competitionHeartbeatDatabaseSnapshot {
	live := server.DbContextBudget{State: "live", HasDeadline: true, RemainingNs: int64(time.Second)}
	return competitionHeartbeatDatabaseSnapshot{AtEntry: live, AtReturn: live, Step: competitionHeartbeatDatabaseCallbackReturn, CallbackEntries: 1,
		Acquire: server.DbAcquireObservation{Attempt: 1, Returned: true, Admitted: true, BeforeOpen: live, BeforeAcquire: live, AtReturn: live,
			PoolReturn: &server.DbAcquirePoolSnapshot{Total: 2}},
		Transaction: server.DbTxObservationSnapshot{AcquireAttempts: 1, AcquireSucceeded: 1, BeginAttempts: 1, BeginSucceeded: 1, CallbackEntered: 1, CommitAttempts: 1, CommitAcknowledged: 1},
		Statements:  []competitionHeartbeatStatementObservation{{Step: competitionHeartbeatDatabaseScopeLock, Ordinal: 1, Kind: "exec", Count: 1, Entered: live, Returned: true, Succeeded: true, AtReturn: &live}}}
}

// Identical final deadline bytes can follow either a nil store reply or a
// returned store error. Only the actual source-owned boundary distinguishes it.
func TestCompetitionHeartbeatOwnerDistinguishesLateSuccessAndStoreError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, succeeded := range []bool{false, true} {
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			parent, cancel := context.WithTimeout(t.Context(), time.Minute)
			expires := time.Now().Add(time.Minute)
			observation := newCompetitionHeartbeatOwnerObservation(parent, expires, expires)
			ack, err := retryCompetitionJobHeartbeatOperationObserved(parent, expires, expires, settings, func(ctx context.Context) error {
				<-ctx.Done()
				if succeeded {
					competitionHeartbeatCompletedDatabaseFromContext(ctx).capture(syntheticHeartbeatOwnerDatabase())
					return nil
				}
				return ctx.Err()
			}, observation)
			value := observation.snapshotCopy()
			cancel()
			if err != context.DeadlineExceeded || !ack.IsZero() || value.StoreCalls != 1 || value.ParentAtReturn.State != "live" || value.AttemptAtReturn.State != "deadline" {
				t.Fatal("owned failure/error identity or actual reply count changed", succeeded, value, err)
			}
			if succeeded {
				if value.Boundary != competitionHeartbeatOwnerAttemptAfterSuccess || value.LastStoreOutcome != competitionHeartbeatStoreSucceeded || value.CompletedDatabaseCaptures != 1 || value.CompletedStoreDatabase == nil || value.CompletedStoreDatabase.Transaction.CommitAcknowledged != 1 {
					t.Fatal("late successful reply was lost or relabeled a store failure", value)
				}
			} else if value.Boundary != competitionHeartbeatOwnerStoreError || value.LastStoreOutcome != competitionHeartbeatStoreFailed || value.CompletedStoreDatabase != nil {
				t.Fatal("plain store error fabricated a completed database reply", value)
			}
			var premature competitionHeartbeatOwnerError
			if errors.As(err, &premature) {
				t.Fatal("adapter wrapped an error before final worker handback")
			}
			wrapped := withCompetitionHeartbeatOwner(err, observation)
			before, after := competitionHeartbeatFailureDetails(err), competitionHeartbeatFailureDetails(wrapped)
			if wrapped.Error() != err.Error() || !errors.Is(wrapped, err) || before.DetailSha256 != after.DetailSha256 || after.HeartbeatDatabase != nil || after.HeartbeatOwner == nil ||
				safeCompetitionHeartbeatError(wrapped).Error() != safeCompetitionHeartbeatError(err).Error() {
				t.Fatal("diagnostics changed original error/digest or backfilled success as error proof")
			}
		}
	})
}

// Wait, parent, authority and total exhaustion are observed at their actual
// returns, not inferred by matching the same deadline leaf afterward.
func TestCompetitionHeartbeatOwnerRecordsWaitParentAuthorityAndTotalBoundaries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, mode := range []string{"wait", "parent-wait", "parent-store", "authority-store", "authority-entry", "parent-entry", "total"} {
			started := time.Now()
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			parent, cancel := context.WithCancel(t.Context())
			job, host := started.Add(time.Minute), started.Add(time.Minute)
			waitFailure := errors.New("synthetic-private-wait-failure")
			want := competitionHeartbeatOwnerWaitError
			wantOutcome, wantCalls := competitionHeartbeatStoreFailed, uint64(1)
			current := started
			switch mode {
			case "parent-wait":
				want = competitionHeartbeatOwnerParentDuringWait
			case "parent-store":
				want, wantOutcome = competitionHeartbeatOwnerParentAfterStore, competitionHeartbeatStoreSucceeded
			case "authority-store":
				settings.LeaseRetry.Now = func() time.Time { return current }
				want, wantOutcome = competitionHeartbeatOwnerAuthorityAfterStore, competitionHeartbeatStoreSucceeded
			case "authority-entry":
				job = started.Add(-time.Second)
				want, wantOutcome, wantCalls = competitionHeartbeatOwnerAuthorityBeforeDispatch, competitionHeartbeatStoreNotCalled, 0
			case "parent-entry":
				cancel()
				want, wantOutcome, wantCalls = competitionHeartbeatOwnerParentBeforeDispatch, competitionHeartbeatStoreNotCalled, 0
			case "total":
				want = competitionHeartbeatOwnerTotalAfterStore
			}
			settings.LeaseRetry.Wait = func(ctx context.Context, _ time.Duration) error {
				if mode == "parent-wait" {
					cancel()
					return ctx.Err()
				}
				return waitFailure
			}
			observation := newCompetitionHeartbeatOwnerObservation(parent, job, host)
			ack, err := retryCompetitionJobHeartbeatOperationObserved(parent, job, host, settings, func(ctx context.Context) error {
				if mode == "parent-store" {
					cancel()
					return nil
				}
				if mode == "authority-store" {
					current = started.Add(2 * time.Minute)
					return nil
				}
				<-ctx.Done()
				if mode == "total" {
					time.Sleep(settings.AcquireRetry.Timeout)
				}
				return competitionHeartbeatJobLockFailure(ctx.Err())
			}, observation)
			value := observation.snapshotCopy()
			cancel()
			if err == nil || !ack.IsZero() || value.Boundary != want || value.StoreCalls != wantCalls || value.LastStoreOutcome != wantOutcome ||
				!value.HasConfirmedJobExpiry || !value.HasConfirmedHostExpiry || value.ConfirmedJobExpiryUnixNs != job.UnixNano() || value.ConfirmedHostExpiryUnixNs != host.UnixNano() {
				t.Fatal("owner return was misclassified or policy bounds changed", mode, value, err)
			}
			if mode == "wait" && err != waitFailure || mode == "total" && (value.TotalAtReturn.State != "deadline" || value.ParentAtReturn.State != "live") {
				t.Fatal("wait identity or independently owned total deadline was lost", mode, value, err)
			}
		}
	})
}

// The completed-deadline predicate sees the exact original operation error,
// while a later nil reply cannot borrow the preceding store's database facts.
func TestCompetitionHeartbeatOwnerKeepsOriginalPredicateAndResetsLastReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Now = time.Now
		expires, calls, proofs := time.Now().Add(time.Minute), 0, 0
		original := errors.New("synthetic-private-original-store-error")
		observation := newCompetitionHeartbeatOwnerObservation(t.Context(), expires, expires)
		ack, err := retryCompetitionHeartbeatWithProofObserved(t.Context(), expires, settings, func(ctx context.Context) error {
			calls++
			if calls == 1 {
				<-ctx.Done()
				competitionHeartbeatCompletedDatabaseFromContext(ctx).capture(syntheticHeartbeatOwnerDatabase())
				return original
			}
			return nil
		}, func(err error) bool {
			proofs++
			if err != original {
				t.Fatal("diagnostic wrapper reached an operational retry predicate")
			}
			return true
		}, observation)
		value := observation.snapshotCopy()
		if err != nil || ack.IsZero() || calls != 2 || proofs != 1 || value.Boundary != competitionHeartbeatOwnerSuccess || value.StoreCalls != 2 ||
			value.LastStoreOutcome != competitionHeartbeatStoreSucceeded || value.CompletedDatabaseCaptures != 0 || value.CompletedStoreDatabase != nil {
			t.Fatal("observer changed retry or borrowed an earlier completed reply", value, err)
		}
	})
}

// No observation adds a policy-clock read or changes a timely ACK/error path.
func TestCompetitionHeartbeatOwnerPreservesPolicyClockAndErrorIdentity(t *testing.T) {
	started := time.Now()
	counts := []int{}
	for _, observed := range []bool{false, true} {
		settings := defaultCompetitionHeartbeatSettings()
		count := 0
		settings.LeaseRetry.Now = func() time.Time { count++; return started }
		expires := started.Add(time.Minute)
		var observation *competitionHeartbeatOwnerObservation
		if observed {
			observation = newCompetitionHeartbeatOwnerObservation(t.Context(), expires, expires)
		}
		ack, err := retryCompetitionJobHeartbeatOperationObserved(t.Context(), expires, expires, settings, func(context.Context) error { return nil }, observation)
		if err != nil || !ack.Equal(started) {
			t.Fatal("observation changed timely success")
		}
		counts = append(counts, count)
	}
	if counts[0] != counts[1] || counts[0] != 5 {
		t.Fatal("observer added policy-clock reads", counts)
	}
}

// Nested slices/pointers and late publication cannot mutate persisted facts;
// private cause/context bytes never enter the structured event projection.
func TestCompetitionHeartbeatOwnerDeepCopyBoundPrivacyAndClosedSink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), struct{}{}, "synthetic-private-context-token"), time.Minute)
	defer cancel()
	expires := time.Now().Add(time.Minute)
	observation := newCompetitionHeartbeatOwnerObservation(ctx, expires, expires)
	sink := &competitionHeartbeatCompletedDatabase{}
	database := syntheticHeartbeatOwnerDatabase()
	observation.beginStore(ctx)
	sink.capture(database)
	observation.finishStore(ctx, false, sink)
	observation.returning(competitionHeartbeatOwnerStoreError, ctx, ctx)
	cause := &pgconn.PgError{Code: "08006", Message: "synthetic-private-password", Where: "synthetic-private-sql"}
	wrapped := withCompetitionHeartbeatOwner(cause, observation)
	before := competitionHeartbeatFailureDetails(wrapped)
	encoded, err := json.Marshal(competitionHandBackEvent{Reason: "heartbeat_failed", competitionHeartbeatFailure: before})
	sum := sha256.Sum256([]byte(cause.Error()))
	var actual *pgconn.PgError
	if err != nil || strings.Contains(string(encoded), "synthetic-private") || wrapped.Error() != cause.Error() || !errors.Is(wrapped, cause) || !errors.As(wrapped, &actual) || actual != cause ||
		before.DetailSha256 != hex.EncodeToString(sum[:]) || before.HeartbeatOwner == nil || before.HeartbeatDatabase != nil {
		t.Fatal("privacy/error identity or distinct completed-reply namespace changed")
	}
	database.Statements[0].Kind = "synthetic-private-changed"
	database.Statements[0].AtReturn.RemainingNs = -1
	database.Acquire.PoolReturn.Total = 99
	sink.capture(database)
	copy := observation.snapshotCopy()
	copy.CompletedStoreDatabase.Statements[0].AtReturn.State = "synthetic-private-mutated-copy"
	copy.CompletedStoreDatabase.Acquire.PoolReturn.Total = 999
	before.HeartbeatOwner.CompletedStoreDatabase.Statements[0].Kind = "synthetic-private-mutated-projection"
	after, err := json.Marshal(competitionHandBackEvent{Reason: "heartbeat_failed", competitionHeartbeatFailure: competitionHeartbeatFailureDetails(wrapped)})
	if err != nil || string(after) != string(encoded) || observation.snapshotCopy().CompletedDatabaseCaptures != 1 {
		t.Fatal("completed facts retained mutable aliases or accepted late publication")
	}
	for _, mode := range []string{"size", "shape", "kind", "budget"} {
		database := syntheticHeartbeatOwnerDatabase()
		switch mode {
		case "size":
			database.Statements = make([]competitionHeartbeatStatementObservation, int(competitionHeartbeatDatabaseStepCount)*3+1)
		case "shape":
			database.SqlShape = "synthetic-private-shape"
		case "kind":
			database.Statements[0].Kind = "synthetic-private-kind"
		case "budget":
			database.AtReturn.State = "synthetic-private-budget"
		}
		sink := &competitionHeartbeatCompletedDatabase{}
		sink.capture(database)
		count, retained, omitted := sink.closeAndSnapshot()
		if count != 1 || retained != nil || !omitted {
			t.Fatal("unbounded/free-text database facts were retained", mode)
		}
	}
}

// Close/capture are linearizable in either real-goroutine ordering. A reply
// published after the close receipt can never change that receipt afterward.
func TestCompetitionHeartbeatOwnerConcurrentCaptureAndCloseAreDetached(t *testing.T) {
	for _, captureFirst := range []bool{false, true} {
		sink := &competitionHeartbeatCompletedDatabase{}
		database := syntheticHeartbeatOwnerDatabase()
		start, published := make(chan struct{}), make(chan struct{})
		go func() { <-start; sink.capture(database); close(published) }()
		if captureFirst {
			close(start)
			<-published
		}
		count, snapshot, omitted := sink.closeAndSnapshot()
		if !captureFirst {
			close(start)
			<-published
		}
		afterCount, after, afterOmitted := sink.closeAndSnapshot()
		if omitted || afterOmitted || count != afterCount || captureFirst && (count != 1 || snapshot == nil || after == nil) ||
			!captureFirst && (count != 0 || snapshot != nil || after != nil) {
			t.Fatal("late concurrent capture changed the completed receipt", captureFirst, count, afterCount)
		}
		if snapshot != nil {
			snapshot.Statements[0].AtReturn.State = "synthetic-mutated-receipt"
			if after.Statements[0].AtReturn.State != "live" {
				t.Fatal("two close receipts shared mutable nested data")
			}
		}
	}
	sink := &competitionHeartbeatCompletedDatabase{}
	sink.stateLock.Lock()
	ready, start, published := make(chan struct{}, 2), make(chan struct{}), make(chan struct{})
	type receipt struct {
		count    uint64
		snapshot *competitionHeartbeatDatabaseSnapshot
		omitted  bool
	}
	closed := make(chan receipt, 1)
	go func() {
		ready <- struct{}{}
		<-start
		sink.capture(syntheticHeartbeatOwnerDatabase())
		close(published)
	}()
	go func() {
		ready <- struct{}{}
		<-start
		count, snapshot, omitted := sink.closeAndSnapshot()
		closed <- receipt{count: count, snapshot: snapshot, omitted: omitted}
	}()
	<-ready
	<-ready
	close(start)
	sink.stateLock.Unlock()
	first := <-closed
	<-published
	count, snapshot, omitted := sink.closeAndSnapshot()
	if first.omitted || omitted || first.count != count || count > 1 || (first.snapshot == nil) != (snapshot == nil) || (count == 0) != (snapshot == nil) {
		t.Fatal("concurrent close/capture changed the linearized receipt", first.count, count)
	}
}

// The live worker attaches owner facts only after joining its evaluator. A nil
// store reply remains success even when the unchanged owner refuses its ACK.
func TestCompetitionHeartbeatOwnerWorkerHandbackAfterLateSuccessPreservesDigest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionLeaseRetryWorkerFixture(t)
		watchdog, stop := context.WithTimeout(t.Context(), time.Minute)
		defer stop()
		fixture.watchdog = watchdog
		fixture.worker.confirmedHostLeaseExpiresAt = fixture.clock.Now().Add(time.Minute)
		fixture.store.heartbeat = func(ctx context.Context, _ int) error { <-ctx.Done(); return nil }
		fixture.Start(t)
		fixture.ticks <- fixture.clock.Now()
		select {
		case <-fixture.evaluator.canceled:
		case <-watchdog.Done():
			t.Fatal("late reply did not stop its original evaluator")
		}
		if fixture.store.reason != "" {
			t.Fatal("owner diagnostics overtook evaluator cleanup")
		}
		err := fixture.Finish(t)
		value := fixture.store.detail.HeartbeatOwner
		plain := competitionHeartbeatFailureDetails(context.DeadlineExceeded)
		if !errors.Is(err, context.DeadlineExceeded) || !fixture.store.joined || fixture.store.reason != "heartbeat_failed" || fixture.store.heartbeatCalls != 1 || fixture.evaluator.calls != 1 ||
			fixture.store.detail.DetailSha256 != plain.DetailSha256 || fixture.store.detail.HeartbeatDatabase != nil || value == nil ||
			value.Boundary != competitionHeartbeatOwnerAttemptAfterSuccess || value.LastStoreOutcome != competitionHeartbeatStoreSucceeded || value.CompletedStoreDatabase != nil {
			t.Fatal("final handback lost reply/boundary/digest or fabricated database proof", err, value)
		}
	})
}
