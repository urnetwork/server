// Per-worker liveness and finished-outcome persistence have independent bounded
// retry policies; failures retain only non-secret diagnostic evidence.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/urnetwork/server"
)

// One worker owns cleanup, liveness retry and separate metadata-persistence
// policies. A non-positive handback timeout retains historical cleanup bounds.
type competitionHeartbeatSettings struct {
	HandBackTimeout time.Duration
	LeaseRetry      competitionLeaseRetrySettings
	// The existing outer allowance admits acquisition-only host failures and
	// the job's separately proved no-COMMIT slot-update failure. It never
	// enlarges lock retries, lease duration or the evaluation clock.
	AcquireRetry competitionHeartbeatAcquireRetrySettings
	// Independent metadata budget; never extends the evaluation or its lease.
	CompletionRetry competitionLeaseRetrySettings
}

// Retains fifteen-second cleanup and independently owned default retry policies.
func defaultCompetitionHeartbeatSettings() competitionHeartbeatSettings {
	return competitionHeartbeatSettings{HandBackTimeout: 15 * time.Second,
		LeaseRetry: defaultCompetitionLeaseRetrySettings(), AcquireRetry: defaultCompetitionHeartbeatAcquireRetrySettings(), CompletionRetry: defaultCompetitionLeaseRetrySettings()}
}

// Only fixed categories, a validated PostgreSQL code, an opaque digest and
// finite heartbeat boundaries are serializable. The original error is absent.
type competitionHeartbeatFailure struct {
	Category          string                                `json:"error_category,omitempty"`
	SqlState          string                                `json:"error_sqlstate,omitempty"`
	DetailSha256      string                                `json:"error_sha256,omitempty"`
	DatabasePhase     server.DbOperationPhase               `json:"database_phase,omitempty"`
	HeartbeatDatabase *competitionHeartbeatDatabaseSnapshot `json:"heartbeat_database,omitempty"`
}

// Adds finite source-boundary evidence without changing formatting or identity.
type competitionDatabasePhaseError struct {
	cause error
	phase server.DbOperationPhase
}

// Preserve the exact original digest and never format the observation as SQL.
func (self competitionDatabasePhaseError) Error() string { return self.cause.Error() }

// Existing cancellation, PostgreSQL and fencing decisions retain their causes.
func (self competitionDatabasePhaseError) Unwrap() error { return self.cause }

// Read the sequential observer only after the complete database call unwinds.
func captureCompetitionDatabasePhase(run func(*server.DbPhaseObservation)) error {
	observation := &server.DbPhaseObservation{}
	err := captureDatabaseError(func() { run(observation) })
	if err == nil {
		return nil
	}
	return competitionDatabasePhaseError{cause: err, phase: observation.Phase()}
}

// A narrow optional capability preserves compatibility with non-PG stores.
// Both direct and embedded production PostgresStore values implement it.
type competitionHeartbeatFailureStore interface {
	handBackWithFailure(context.Context, string, server.Id, string, competitionHeartbeatFailure) error
}

// Classification is diagnostic only, never permission to retain a lost lease.
func competitionHeartbeatFailureDetails(err error) competitionHeartbeatFailure {
	if err == nil {
		return competitionHeartbeatFailure{}
	}
	digest := sha256.Sum256([]byte(err.Error()))
	detail := competitionHeartbeatFailure{Category: "other", DetailSha256: hex.EncodeToString(digest[:])}
	var phaseError competitionDatabasePhaseError
	if errors.As(err, &phaseError) && phaseError.phase > server.DbOperationUnknown && phaseError.phase <= server.DbOperationPostCommit {
		detail.DatabasePhase = phaseError.phase
	}
	var heartbeatError competitionHeartbeatDatabaseError
	if errors.As(err, &heartbeatError) {
		snapshot := heartbeatError.snapshot
		detail.HeartbeatDatabase = &snapshot
	}
	var sqlError interface{ SQLState() string }
	var networkError net.Error
	switch {
	case errors.Is(err, ErrLeaseLost):
		detail.Category = "lease_lost"
	case errors.Is(err, context.DeadlineExceeded):
		detail.Category = "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		detail.Category = "canceled"
	case errors.As(err, &sqlError):
		detail.Category = "postgresql"
		state := sqlError.SQLState()
		valid := len(state) == 5
		for _, value := range state {
			valid = valid && (value >= '0' && value <= '9' || value >= 'A' && value <= 'Z')
		}
		if valid {
			detail.SqlState = state
		}
	case errors.As(err, &networkError):
		detail.Category = "network"
	}
	return detail
}

// The unchanged reason remains top-level; no error message or error object can
// reach appendEvent's JSON encoder through this payload.
type competitionHandBackEvent struct {
	Reason string `json:"reason"`
	competitionHeartbeatFailure
}

// Returned errors remain inspectable with errors.Is/As but safe to format.
type competitionSafeHeartbeatError struct {
	detail competitionHeartbeatFailure
	cause  error
}

// Diagnostic formatting contains only the same safe fields as the ledger.
func (self competitionSafeHeartbeatError) Error() string {
	formatted := fmt.Sprintf("category=%s sqlstate=%s detail_sha256=%s", self.detail.Category, self.detail.SqlState, self.detail.DetailSha256)
	if self.detail.DatabasePhase != server.DbOperationUnknown {
		formatted += fmt.Sprintf(" database_phase=%d", self.detail.DatabasePhase)
	}
	if observation := self.detail.HeartbeatDatabase; observation != nil {
		formatted += fmt.Sprintf(" heartbeat_step=%d callback_entries=%d acquire_attempts=%d acquire_succeeded=%d commit_acknowledged=%d rollback_acknowledged=%d",
			observation.Step, observation.CallbackEntries, observation.Transaction.AcquireAttempts, observation.Transaction.AcquireSucceeded,
			observation.Transaction.CommitAcknowledged, observation.Transaction.RollbackAcknowledged)
	}
	return formatted
}

// The original cause is retained for programmatic decisions, never serialization.
func (self competitionSafeHeartbeatError) Unwrap() error { return self.cause }

// This wrapper does not classify a failure as retryable or suppress any error.
func safeCompetitionHeartbeatError(err error) error {
	return competitionSafeHeartbeatError{detail: competitionHeartbeatFailureDetails(err), cause: err}
}
