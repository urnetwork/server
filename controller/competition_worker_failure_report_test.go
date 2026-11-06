// Worker failure reports preserve finite phase/custody evidence while
// excluding private error payloads and refusing every unknown schema string.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Both heartbeat and supervisor-read errors retain their actual fixed counts,
// not a lossy exit code or an inference from the private error's text.
func TestCompetitionWorkerFailureReportPreservesFiniteDatabaseEvidence(t *testing.T) {
	proof := competitionSupervisorReadLockFailure()
	proof.cause = competitionDatabasePhaseError{cause: &pgconn.PgError{Code: "08P01", Message: "synthetic-private-database-payload"}, phase: server.DbOperationCallback}
	content, err := MarshalCompetitionWorkerFailureReport(proof)
	if err != nil {
		t.Fatal(err)
	}
	report, err := DecodeCompetitionWorkerFailureReport(content)
	if err != nil || report.Category != "postgresql" || report.SqlState != "08P01" || report.DatabasePhase != server.DbOperationCallback ||
		report.SupervisorStep != uint8(competitionSupervisorRenewalLeaseRowLock) || report.CallbackEntries != 1 || report.CommitAttempts != 0 ||
		report.RollbackAcknowledged != 1 || report.ReleaseReturned != 1 || report.ReleaseDisposition != server.DbTxReleaseIdle || report.CleanupComplete ||
		strings.Contains(string(content), "synthetic-private-database-payload") {
		t.Fatal("worker report lost finite facts or reflected private data", report, err)
	}
	heartbeat := competitionHeartbeatDatabaseError{cause: proof.cause, snapshot: competitionHeartbeatDatabaseSnapshot{
		Step: competitionHeartbeatDatabaseJobLock, CallbackEntries: 1, Transaction: proof.transaction}}
	content, err = MarshalCompetitionWorkerFailureReport(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	report, err = DecodeCompetitionWorkerFailureReport(content)
	if err != nil || report.HeartbeatStep != uint8(competitionHeartbeatDatabaseJobLock) || report.SupervisorStep != 0 || report.RollbackAcknowledged != 1 {
		t.Fatal("worker report borrowed another owner's observation", report, err)
	}
}

// All reflected text is validated, even when it arrived from a pinned child.
// Unknown keys, suffixes, enum values and arbitrary error text are refused.
func TestCompetitionWorkerFailureReportRejectsUnvalidatedDiagnostics(t *testing.T) {
	content, err := MarshalCompetitionWorkerFailureReport(context.DeadlineExceeded)
	if err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	if err := json.Unmarshal(content, &original); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		key   string
		value any
	}{
		{key: "schema", value: 2}, {key: "kind", value: "synthetic-private-kind"},
		{key: "category", value: "synthetic-private-category"}, {key: "sqlstate", value: "secret"},
		{key: "detail_sha256", value: "synthetic-private-digest"}, {key: "database_phase", value: 255},
		{key: "heartbeat_step", value: 255}, {key: "supervisor_step", value: 255},
		{key: "owner_boundary", value: 255}, {key: "release_disposition", value: 255},
		{key: "error", value: "synthetic-private-cause"},
	} {
		mutated := map[string]any{}
		for key, value := range original {
			mutated[key] = value
		}
		mutated[check.key] = check.value
		encoded, err := json.Marshal(mutated)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeCompetitionWorkerFailureReport(encoded); err == nil {
			t.Error("unvalidated worker field escaped", check.key)
		}
	}
	for _, encoded := range [][]byte{append(append([]byte{}, content...), []byte(" {}")...), []byte(strings.Repeat(" ", 8*1024+1)), []byte("null")} {
		if _, err := DecodeCompetitionWorkerFailureReport(encoded); err == nil {
			t.Error("unbounded or trailing diagnostic accepted")
		}
	}
	if _, err := MarshalCompetitionWorkerFailureReport(nil); err == nil {
		t.Fatal("healthy worker manufactured a failure report")
	}
}

// Formatting a generic private error exposes only its digest and category.
func TestCompetitionWorkerFailureReportNeverReflectsPrivateErrorBytes(t *testing.T) {
	const private = "synthetic-private-worker-log"
	content, err := MarshalCompetitionWorkerFailureReport(errors.New(private))
	if err != nil || strings.Contains(string(content), private) {
		t.Fatal("worker report reflected private error bytes", err)
	}
	report, err := DecodeCompetitionWorkerFailureReport(content)
	if err != nil || report.Category != "other" || len(report.DetailSha256) != 64 {
		t.Fatal("generic worker failure lost safe finite evidence", err)
	}
}
