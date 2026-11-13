// A worker exits with a finite diagnostic document, not raw stderr. The
// supervisor validates this closed schema before exposing it in its report.
package controller

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/urnetwork/server/v2026"
)

// All strings are fixed categories, validated codes or an opaque digest. No
// SQL, arguments, private errors, job identifiers or candidate logs can escape.
type CompetitionWorkerFailureReport struct {
	Schema               int                           `json:"schema"`
	Kind                 string                        `json:"kind"`
	Category             string                        `json:"category"`
	SqlState             string                        `json:"sqlstate,omitempty"`
	DetailSha256         string                        `json:"detail_sha256"`
	DatabasePhase        server.DbOperationPhase       `json:"database_phase,omitempty"`
	HeartbeatStep        uint8                         `json:"heartbeat_step,omitempty"`
	SupervisorStep       uint8                         `json:"supervisor_step,omitempty"`
	OwnerBoundary        uint8                         `json:"owner_boundary,omitempty"`
	StoreCalls           uint64                        `json:"store_calls,omitempty"`
	CallbackEntries      uint64                        `json:"callback_entries,omitempty"`
	AcquireAttempts      uint64                        `json:"acquire_attempts,omitempty"`
	AcquireSucceeded     uint64                        `json:"acquire_succeeded,omitempty"`
	BeginAttempts        uint64                        `json:"begin_attempts,omitempty"`
	BeginSucceeded       uint64                        `json:"begin_succeeded,omitempty"`
	CommitAttempts       uint64                        `json:"commit_attempts,omitempty"`
	CommitAcknowledged   uint64                        `json:"commit_acknowledged,omitempty"`
	RollbackAttempts     uint64                        `json:"rollback_attempts,omitempty"`
	RollbackAcknowledged uint64                        `json:"rollback_acknowledged,omitempty"`
	ReleaseAttempts      uint64                        `json:"release_attempts,omitempty"`
	ReleaseReturned      uint64                        `json:"release_returned,omitempty"`
	ReleaseDisposition   server.DbTxReleaseDisposition `json:"release_disposition,omitempty"`
	CleanupComplete      bool                          `json:"cleanup_complete,omitempty"`
}

// The original cause remains private; this projection cannot grant replay or
// replace an error's Is/As graph in the worker's actual control flow.
func MarshalCompetitionWorkerFailureReport(err error) ([]byte, error) {
	if err == nil {
		return nil, errors.New("worker failure report requires an error")
	}
	detail := competitionHeartbeatFailureDetails(err)
	report := CompetitionWorkerFailureReport{Schema: 1, Kind: "competition-worker-failure/v1", Category: detail.Category,
		SqlState: detail.SqlState, DetailSha256: detail.DetailSha256, DatabasePhase: detail.DatabasePhase}
	var transaction server.DbTxObservationSnapshot
	database := detail.HeartbeatDatabase
	if owner := detail.HeartbeatOwner; owner != nil {
		report.OwnerBoundary, report.StoreCalls = uint8(owner.Boundary), owner.StoreCalls
		if database == nil {
			database = owner.CompletedStoreDatabase
		}
	}
	if database != nil {
		report.HeartbeatStep, report.CallbackEntries = uint8(database.Step), database.CallbackEntries
		transaction = database.Transaction
	}
	var supervisor competitionSupervisorRenewError
	if errors.As(err, &supervisor) {
		report.SupervisorStep, report.CallbackEntries = uint8(supervisor.step), supervisor.callbacks
		transaction = supervisor.transaction
	}
	report.AcquireAttempts, report.AcquireSucceeded = transaction.AcquireAttempts, transaction.AcquireSucceeded
	report.BeginAttempts, report.BeginSucceeded = transaction.BeginAttempts, transaction.BeginSucceeded
	report.CommitAttempts, report.CommitAcknowledged = transaction.CommitAttempts, transaction.CommitAcknowledged
	report.RollbackAttempts, report.RollbackAcknowledged = transaction.RollbackAttempts, transaction.RollbackAcknowledged
	report.ReleaseAttempts, report.ReleaseReturned = transaction.ReleaseAttempts, transaction.ReleaseReturned
	report.ReleaseDisposition, report.CleanupComplete = transaction.LastRelease.Disposition, transaction.LastRelease.CleanupComplete
	return json.Marshal(report)
}

// A finite schema is necessary even for a trusted process: arbitrary log
// lines and unknown fields must never be reflected as safe diagnostics.
func DecodeCompetitionWorkerFailureReport(content []byte) (*CompetitionWorkerFailureReport, error) {
	if len(content) > 8*1024 {
		return nil, errors.New("worker failure report exceeds its schema bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var report CompetitionWorkerFailureReport
	if err := decoder.Decode(&report); err != nil {
		return nil, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF || report.Schema != 1 || report.Kind != "competition-worker-failure/v1" ||
		report.DatabasePhase > server.DbOperationPostCommit || report.HeartbeatStep >= uint8(competitionHeartbeatDatabaseStepCount) ||
		report.SupervisorStep > uint8(competitionSupervisorWorkerFenceRead) || report.OwnerBoundary > uint8(competitionHeartbeatOwnerSuccess) ||
		report.ReleaseDisposition > server.DbTxReleaseDestroyTransferred {
		return nil, errors.New("worker failure report schema is invalid")
	}
	switch report.Category {
	case "lease_lost", "deadline_exceeded", "canceled", "postgresql", "network", "other":
	default:
		return nil, errors.New("worker failure category is invalid")
	}
	digest, err := hex.DecodeString(report.DetailSha256)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != report.DetailSha256 {
		return nil, errors.New("worker failure digest is invalid")
	}
	if report.SqlState != "" {
		if len(report.SqlState) != 5 {
			return nil, errors.New("worker failure SQL state is invalid")
		}
		for _, value := range report.SqlState {
			if !(value >= 'A' && value <= 'Z' || value >= '0' && value <= '9') {
				return nil, errors.New("worker failure SQL state is invalid")
			}
		}
	}
	return &report, nil
}
