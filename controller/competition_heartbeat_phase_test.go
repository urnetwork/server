// Finite database-phase evidence must preserve cancellation and private causes.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Distinct operation boundaries remain distinguishable without leaking text.
func TestCompetitionHeartbeatPhasePreservesCauseAndDigest(t *testing.T) {
	cause := errors.Join(errors.New("synthetic-private-detail"), context.DeadlineExceeded)
	digest := sha256.Sum256([]byte(cause.Error()))
	for phase := server.DbOperationAcquire; phase <= server.DbOperationPostCommit; phase++ {
		wrapped := competitionDatabasePhaseError{cause: cause, phase: phase}
		detail := competitionHeartbeatFailureDetails(wrapped)
		if detail.DatabasePhase != phase || detail.Category != "deadline_exceeded" || detail.DetailSha256 != hex.EncodeToString(digest[:]) || !errors.Is(wrapped, context.DeadlineExceeded) {
			t.Fatalf("phase evidence lost cause or identity: %+v", detail)
		}
		encoded, err := json.Marshal(competitionHandBackEvent{Reason: "heartbeat_failed", competitionHeartbeatFailure: detail})
		if err != nil || strings.Contains(string(encoded), "synthetic-private-detail") || !strings.Contains(string(encoded), "database_phase") {
			t.Fatal("phase evidence missing or private detail serialized")
		}
		safe := safeCompetitionHeartbeatError(wrapped)
		if !strings.Contains(safe.Error(), "database_phase=") || strings.Contains(safe.Error(), "synthetic-private-detail") || !errors.Is(safe, context.DeadlineExceeded) {
			t.Fatal("safe worker diagnostic lost phase or exposed private cause")
		}
	}
}

// Unknown/future numeric values are not admitted as invented phase evidence.
func TestCompetitionHeartbeatPhaseRejectsUnknownBoundary(t *testing.T) {
	for _, phase := range []server.DbOperationPhase{server.DbOperationUnknown, server.DbOperationPhase(255)} {
		detail := competitionHeartbeatFailureDetails(competitionDatabasePhaseError{cause: ErrLeaseLost, phase: phase})
		if detail.DatabasePhase != server.DbOperationUnknown || detail.Category != "lease_lost" {
			t.Fatalf("invalid phase or lost fencing identity: %+v", detail)
		}
		encoded, err := json.Marshal(detail)
		if err != nil || strings.Contains(string(encoded), "database_phase") {
			t.Fatal("unknown boundary serialized as observed phase")
		}
	}
}

// Capturing errors observes unwind without swallowing panics or success.
func TestCompetitionHeartbeatPhaseCapturePreservesUnwind(t *testing.T) {
	if err := captureCompetitionDatabasePhase(func(*server.DbPhaseObservation) {}); err != nil {
		t.Fatal(err)
	}
	err := captureCompetitionDatabasePhase(func(*server.DbPhaseObservation) { panic(context.Canceled) })
	if !errors.Is(err, context.Canceled) || competitionHeartbeatFailureDetails(err).Category != "canceled" {
		t.Fatal("capture lost cancellation identity")
	}
}

// Observational wrapping must not suppress the existing completed-lock retry.
func TestCompetitionHeartbeatPhasePreservesLockRetry(t *testing.T) {
	now := time.Now()
	attempts := 0
	settings := competitionLeaseRetrySettings{
		Timeout: time.Minute, Interval: time.Second, MaximumAttempts: 2,
		Now:  func() time.Time { return now },
		Wait: func(context.Context, time.Duration) error { return nil },
	}
	_, err := retryCompetitionLeaseOperation(context.Background(), now.Add(time.Hour), settings, func(context.Context) error {
		attempts++
		if attempts == 1 {
			return competitionDatabasePhaseError{cause: &pgconn.PgError{Code: "55P03"}, phase: server.DbOperationCallback}
		}
		return nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("phase wrapping changed completed-lock retry: attempts=%d err=%v", attempts, err)
	}
}

// A mixed cancellation/fencing cause must never become a retryable lock error.
func TestCompetitionHeartbeatPhasePreservesTerminalRetryCauses(t *testing.T) {
	now := time.Now()
	for _, terminal := range []error{context.Canceled, context.DeadlineExceeded, ErrLeaseLost} {
		attempts := 0
		settings := competitionLeaseRetrySettings{
			Timeout: time.Minute, Interval: time.Second, MaximumAttempts: 2,
			Now: func() time.Time { return now },
			Wait: func(context.Context, time.Duration) error {
				t.Fatal("terminal phase-wrapped cause reached retry wait")
				return nil
			},
		}
		_, err := retryCompetitionLeaseOperation(context.Background(), now.Add(time.Hour), settings, func(context.Context) error {
			attempts++
			return competitionDatabasePhaseError{
				cause: errors.Join(&pgconn.PgError{Code: "55P03"}, terminal), phase: server.DbOperationCallback,
			}
		})
		if attempts != 1 || !errors.Is(err, terminal) {
			t.Fatalf("phase wrapping changed terminal cause: attempts=%d err=%v", attempts, err)
		}
	}
}
