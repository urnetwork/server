// Exact pooler refusals need source-owned no-callback proof. Synthetic graphs
// force both the healthy continuation and ambiguous-history rejection paths.
package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Synthetic refused acquisition/BEGIN never claims a successful server call.
func competitionPoolerRefusalFailure(begin bool) competitionSupervisorRenewError {
	cause := &pgconn.PgError{Code: "08P01", Severity: "FATAL", Message: "query_wait_timeout"}
	proof := competitionSupervisorAcquireFailure(cause)
	live := proof.acquire.BeforeAcquire
	proof.acquire.AtReturn = live
	proof.transaction.AcquireAttempts = 1
	if begin {
		proof.phase = server.DbOperationBegin
		proof.cause = competitionDatabasePhaseError{cause: cause, phase: proof.phase}
		proof.acquire.Admitted, proof.read.AcquireSucceeded = true, 1
		proof.transaction.AcquireSucceeded, proof.transaction.BeginAttempts = 1, 1
		proof.transaction.LastBegin = server.DbTxBoundaryObservation{Before: live, AtReturn: live, Returned: true}
		proof.transaction.ReleaseAttempts, proof.transaction.ReleaseReturned = 1, 1
		proof.transaction.LastRelease.Disposition = server.DbTxReleaseDestroyTransferred
	}
	return proof
}

// Removing only the pooler predicate restores the incident's immediate
// cancellation; the unchanged old lock allowance must admit one healthy call.
func TestCompetitionLivenessPoolerRefusalRecoversBeforeCallback(t *testing.T) {
	for _, begin := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			settings := defaultCompetitionSupervisorRenewalSettings()
			started, calls := time.Now(), 0
			err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(time.Minute), settings, func(context.Context) error {
				calls++
				if calls == 1 {
					return competitionPoolerRefusalFailure(begin)
				}
				return nil
			})
			if err != nil || calls != 2 || time.Since(started) != settings.LockRetry.Interval {
				t.Fatal("completed pooler refusal canceled live renewal", begin, calls, time.Since(started), err)
			}
		})
	}
}

// Raw SQLSTATE, callback entry, uncertain cleanup and possible commit can
// never borrow the no-business-work proof or the owned-deadline allowance.
func TestCompetitionLivenessPoolerRefusalRejectsUnknownOrAdmittedWork(t *testing.T) {
	for _, check := range []struct {
		name   string
		mutate func(*competitionSupervisorRenewError)
	}{
		{name: "callback entered", mutate: func(p *competitionSupervisorRenewError) { p.callbackEntered = true }},
		{name: "callback census", mutate: func(p *competitionSupervisorRenewError) { p.transaction.CallbackEntered = 1 }},
		{name: "successful begin", mutate: func(p *competitionSupervisorRenewError) { p.transaction.BeginSucceeded = 1 }},
		{name: "possible commit", mutate: func(p *competitionSupervisorRenewError) { p.transaction.CommitAttempts = 1 }},
		{name: "acknowledged commit", mutate: func(p *competitionSupervisorRenewError) { p.transaction.CommitAcknowledged = 1 }},
		{name: "rollback history", mutate: func(p *competitionSupervisorRenewError) { p.transaction.RollbackAttempts = 1 }},
		{name: "prior admitted attempt", mutate: func(p *competitionSupervisorRenewError) { p.transaction.AcquireSucceeded = 2 }},
		{name: "release pending", mutate: func(p *competitionSupervisorRenewError) { p.transaction.ReleaseReturned = 0 }},
		{name: "unknown custody", mutate: func(p *competitionSupervisorRenewError) {
			p.transaction.LastRelease.Disposition = server.DbTxReleaseUnknown
		}},
		{name: "begin not returned", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastBegin.Returned = false }},
		{name: "expired return", mutate: func(p *competitionSupervisorRenewError) { p.acquire.AtReturn.State = "deadline" }},
		{name: "callback phase", mutate: func(p *competitionSupervisorRenewError) { p.phase = server.DbOperationCallback }},
	} {
		proof := competitionPoolerRefusalFailure(true)
		check.mutate(&proof)
		if competitionLivenessPoolerRefusal(proof) {
			t.Errorf("%s granted continuation", check.name)
		}
	}
	proof := competitionPoolerRefusalFailure(true)
	for _, cause := range []error{
		&pgconn.PgError{Code: "08P01", Severity: "FATAL", Message: "synthetic different protocol error"},
		&pgconn.PgError{Code: "08P01", Severity: "ERROR", Message: "query_wait_timeout"},
		&pgconn.PgError{Code: "08006", Severity: "FATAL", Message: "query_wait_timeout"},
		context.Canceled, ErrLeaseLost, errors.New("query_wait_timeout"),
		errors.Join(&pgconn.PgError{Code: "08P01", Severity: "FATAL", Message: "query_wait_timeout"}, context.DeadlineExceeded),
	} {
		proof.cause = competitionDatabasePhaseError{cause: cause, phase: proof.phase}
		if competitionLivenessPoolerRefusal(proof) {
			t.Error("unknown/mixed cause granted continuation", cause)
		}
	}
	proof = competitionPoolerRefusalFailure(true)
	for _, cause := range []error{proof.cause, fmt.Errorf("synthetic outer: %w", proof), errors.Join(proof, context.Canceled)} {
		if competitionLivenessPoolerRefusal(cause) {
			t.Error("unenclosed cause granted continuation")
		}
	}
}

// Fast failures retain the old shared call cap, and failures consuming its
// original allowance cannot obtain the twenty-second deadline recovery reserve.
func TestCompetitionLivenessPoolerRefusalPreservesOriginalBudgetAndCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, consumeAllowance := range []bool{false, true} {
			settings := defaultCompetitionSupervisorRenewalSettings()
			started, calls := time.Now(), 0
			err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(time.Minute), settings, func(context.Context) error {
				calls++
				if consumeAllowance {
					<-time.After(settings.LockRetry.Timeout)
				}
				return competitionPoolerRefusalFailure(true)
			})
			wantCalls := settings.LockRetry.MaximumAttempts
			if consumeAllowance {
				wantCalls = 1
			}
			if err == nil || calls != wantCalls || time.Since(started) > settings.LockRetry.Timeout {
				t.Fatal("pooler refusal enlarged the original allowance/cap", consumeAllowance, calls, time.Since(started), err)
			}
		}
	})
}

// The heartbeat adapters receive their own complete no-callback evidence;
// neither an admitted host write nor another store's raw PgError qualifies.
func TestCompetitionLivenessPoolerRefusalKeepsHeartbeatProofsSeparate(t *testing.T) {
	supervisor := competitionPoolerRefusalFailure(false)
	heartbeat := competitionHeartbeatDatabaseError{cause: supervisor.cause, snapshot: competitionHeartbeatDatabaseSnapshot{
		Acquire: supervisor.acquire, Transaction: supervisor.transaction}}
	host := competitionHostHeartbeatAcquireError{cause: supervisor.cause, acquire: supervisor.acquire, read: supervisor.read}
	for _, cause := range []error{heartbeat, host} {
		if !competitionLivenessPoolerRefusal(cause) {
			t.Error("complete owner-local no-callback refusal did not qualify", cause)
		}
	}
	heartbeat.snapshot.CallbackEntries = 1
	host.callbackEntered = true
	for _, cause := range []error{heartbeat, host, supervisor.cause} {
		if competitionLivenessPoolerRefusal(cause) {
			t.Error("unproven/admitted heartbeat gained pooler continuation", cause)
		}
	}
}
