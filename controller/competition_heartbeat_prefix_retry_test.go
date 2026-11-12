// Exact distributed pre-business prefixes may spend only their original
// confirmed owner reserve. Every other authority path retains its old policy.
package controller

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Synthetic source proof has one final failed statement and no business
// update; all diagnostic positions preserve capture's fixed step order.
func competitionHeartbeatPrefixFailure(count int) competitionHeartbeatDatabaseError {
	proof := competitionHeartbeatDistributedSlotFailure()
	expected := competitionHeartbeatDistributedPreBusinessStatements()[:count]
	proof.snapshot.SqlStatements = uint64(count)
	proof.snapshot.Step = expected[count-1].step
	proof.snapshot.Statements = nil
	ordinals := map[competitionHeartbeatDatabaseStep]uint8{}
	for index, statement := range expected {
		ordinals[statement.step]++
		returned := proof.snapshot.AtEntry
		if index == count-1 {
			returned = proof.snapshot.AtReturn
		}
		proof.snapshot.Statements = append(proof.snapshot.Statements, competitionHeartbeatStatementObservation{Step: statement.step,
			Ordinal: ordinals[statement.step], Kind: statement.kind, Count: 1, Entered: proof.snapshot.AtEntry,
			Returned: true, Succeeded: index != count-1, AtReturn: &returned})
	}
	return proof
}

// Includes the observed mechanism at the eighth source statement, without
// copying a production identity, timestamp or private observation payload.
func TestCompetitionHeartbeatPrefixProofAcceptsOnlyReviewedPreBusinessPositions(t *testing.T) {
	for count := 1; count <= len(competitionHeartbeatDistributedPreBusinessStatements()); count++ {
		proof := competitionHeartbeatPrefixFailure(count)
		if !competitionHeartbeatDistributedPreBusinessDeadline(proof) || competitionHeartbeatDistributedSlotUpdateDeadline(proof) || competitionHeartbeatSlotUpdateDeadline(proof) {
			t.Fatal("exact read/config prefix was rejected or mislabeled", count)
		}
	}
	observed := competitionHeartbeatPrefixFailure(8)
	last := observed.snapshot.Statements[7]
	if last.Step != competitionHeartbeatDatabaseSupervisorAuthority || last.Ordinal != 2 || last.Kind != "exec" || last.Succeeded {
		t.Fatal("synthetic prefix does not exercise the supervisor configuration deadline")
	}
	if competitionHeartbeatDistributedPreBusinessDeadline(competitionHeartbeatDistributedSlotFailure()) {
		t.Fatal("post-business slot failure borrowed read/config-prefix authority")
	}
}

// The old proof/call ceiling loses custody on the third returned deadline
// despite a live original reserve. The fourth dispatch must use that reserve.
func TestCompetitionHeartbeatPrefixRetryFourthAttemptUsesOriginalReserve(t *testing.T) {
	for _, mode := range []string{"old-proof", "old-cap", "fixed"} {
		synctest.Test(t, func(t *testing.T) {
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			settings.AcquireRetry.AttemptTimeout = 2 * time.Second
			if mode == "old-proof" {
				settings.PrefixMaximumAttempts = 0
			}
			if mode == "old-cap" {
				settings.PrefixMaximumAttempts = settings.LeaseRetry.MaximumAttempts
			}
			started, calls := time.Now(), 0
			ack, err := retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), started.Add(time.Minute), settings, func(attempt context.Context) error {
				calls++
				if calls <= 2 {
					time.Sleep(time.Second)
					return competitionHeartbeatScopeFailure()
				}
				if calls == 3 {
					<-attempt.Done()
					return competitionHeartbeatPrefixFailure(8)
				}
				return nil
			})
			if mode != "fixed" {
				if err == nil || calls != 3 || !ack.IsZero() || time.Since(started) >= settings.AcquireRetry.Timeout {
					t.Fatal("old prefix refusal did not reproduce premature custody loss", calls, err)
				}
			} else if err != nil || calls != 4 || ack.IsZero() || time.Since(started) >= settings.AcquireRetry.Timeout {
				t.Fatal("fourth proved dispatch lost its unchanged owner reserve", calls, err)
			}
		})
	}
}

// Enabling the new prefix class must not implicitly enable a disabled first
// lock extension through the optional shared engine capability.
func TestCompetitionHeartbeatPrefixRetryDoesNotEnableDisabledScopeContinuation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Now = time.Now
		settings.ScopeLockMaximumAttempts = 0
		settings.LeaseRetry.MaximumAttempts = 8
		settings.PrefixMaximumAttempts = 12
		started, calls := time.Now(), 0
		ack, err := retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), started.Add(time.Minute), settings, func(context.Context) error {
			calls++
			time.Sleep(3 * time.Second)
			return competitionHeartbeatScopeFailure()
		})
		if err == nil || !ack.IsZero() || calls != 3 || time.Since(started) >= settings.AcquireRetry.Timeout {
			t.Fatal("prefix capability enabled disabled scope-lock reserve", calls, err)
		}
	})
}

// The extension is cumulative across different reviewed prefix positions; it
// cannot reset either the call count or the original total deadline.
func TestCompetitionHeartbeatPrefixRetryKeepsFiniteCumulativeCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Now = time.Now
		settings.AcquireRetry.AttemptTimeout = time.Second / 2
		started, calls := time.Now(), 0
		ack, err := retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), started.Add(time.Minute), settings, func(attempt context.Context) error {
			calls++
			<-attempt.Done()
			return competitionHeartbeatPrefixFailure(1 + calls%len(competitionHeartbeatDistributedPreBusinessStatements()))
		})
		if err == nil || !ack.IsZero() || calls != settings.PrefixMaximumAttempts || time.Since(started) > settings.AcquireRetry.Timeout {
			t.Fatal("reviewed prefixes created a new call or time allowance", calls, err)
		}
	})
}

// Prefix proof cannot replace either lease or turn cancellation/late success
// into authority. A shared live parent is distinct from an expired child.
func TestCompetitionHeartbeatPrefixRetryRequiresBothAuthoritiesAndTimelyAck(t *testing.T) {
	for _, mode := range []string{"unknown-job", "unknown-host", "expired-job", "expired-host", "parent-cancel", "late-success", "expired-during-return"} {
		synctest.Test(t, func(t *testing.T) {
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			settings.AcquireRetry.AttemptTimeout = time.Second
			started := time.Now()
			job, host := started.Add(time.Minute), started.Add(time.Minute)
			if mode == "unknown-job" {
				job = time.Time{}
			}
			if mode == "unknown-host" {
				host = time.Time{}
			}
			if mode == "expired-job" {
				job = started
			}
			if mode == "expired-host" {
				host = started
			}
			if mode == "expired-during-return" {
				host = started.Add(time.Second)
			}
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			ack, err := retryCompetitionJobHeartbeatOperation(parent, job, host, settings, func(attempt context.Context) error {
				calls++
				if mode == "parent-cancel" {
					cancel()
					return competitionHeartbeatPrefixFailure(8)
				}
				<-attempt.Done()
				if mode == "late-success" {
					return nil
				}
				return competitionHeartbeatPrefixFailure(8)
			})
			maximum := 1
			if mode == "expired-job" || mode == "expired-host" {
				maximum = 0
			}
			if err == nil || !ack.IsZero() || calls != maximum {
				t.Fatal("prefix proof bypassed missing/expired authority or accepted a late acknowledgement", mode, calls, err)
			}
		})
	}
}

// Unknown causes, admitted transactions, manual/nested SQL and even one
// commit attempt invalidate the entire completed-store proof.
func TestCompetitionHeartbeatPrefixProofRejectsAmbiguousHistory(t *testing.T) {
	for index, mutate := range []func(*competitionHeartbeatDatabaseError){
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlShape = competitionHeartbeatLegacySlotShape },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlShapeValid = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlStatements = 0 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlStatements++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.CallbackEntries++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.UnknownStatements++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Step = competitionHeartbeatDatabaseJobUpdate },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CommitAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CommitAcknowledged++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastCommit.Returned = true },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CallbackEntered++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.BeginAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.RollbackAcknowledged = 0 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastRollback.Returned = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.ReleaseReturned = 0 },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Transaction.LastRelease.Disposition = server.DbTxReleaseDestroyTransferred
		},
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.AtReturn = p.snapshot.AtEntry },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements = p.snapshot.Statements[:7] },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[4].Ordinal = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[4].Succeeded = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[4].Count++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[4].Kind = "exec" },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[7].Succeeded = true },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[7].AtReturn = nil },
		func(p *competitionHeartbeatDatabaseError) {
			p.cause = competitionDatabasePhaseError{cause: context.DeadlineExceeded, phase: server.DbOperationCommit}
		},
		func(p *competitionHeartbeatDatabaseError) {
			p.cause = competitionDatabasePhaseError{cause: errors.Join(context.DeadlineExceeded, ErrLeaseLost), phase: server.DbOperationCallback}
		},
	} {
		proof := competitionHeartbeatPrefixFailure(8)
		mutate(&proof)
		if competitionHeartbeatDistributedPreBusinessDeadline(proof) {
			t.Fatal("ambiguous prefix authorized continuation", index)
		}
	}
	for _, cause := range []error{context.Canceled, ErrLeaseLost, &pgconn.PgError{Code: "55P03"}, errors.Join(context.DeadlineExceeded, context.Canceled),
		&net.OpError{Op: "read", Net: "synthetic", Err: context.DeadlineExceeded}, server.NewErrorCauseBatch([]error{context.DeadlineExceeded, nil})} {
		proof := competitionHeartbeatPrefixFailure(8)
		proof.cause = competitionDatabasePhaseError{cause: cause, phase: server.DbOperationCallback}
		if competitionHeartbeatDistributedPreBusinessDeadline(proof) {
			t.Fatalf("unknown prefix cause accepted: %T", cause)
		}
	}
	proof := competitionHeartbeatPrefixFailure(8)
	if competitionHeartbeatDistributedPreBusinessDeadline(errors.Join(proof, context.DeadlineExceeded)) || competitionHeartbeatDistributedPreBusinessDeadline(&proof) ||
		competitionHostHeartbeatUnadmittedDeadline(proof) || competitionSupervisorUnadmittedDeadline(proof) {
		t.Fatal("different root or authority borrowed prefix permission")
	}
}
