// Synthetic invocation facts exercise the actual machine-report serializer
// and unchanged renewal retry engine without a database or wall-clock sleeps.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
)

// These finite synthetic facts are not evidence of an actual database call.
func competitionSupervisorDiagnosticFailure() competitionSupervisorRenewError {
	proof := competitionSupervisorAcquireFailure(context.DeadlineExceeded)
	proof.database = &CompetitionSupervisorRenewalDatabaseFailure{Schema: 1, Kind: "competition-supervisor-renewal-database/v1",
		DatabasePhase: server.DbOperationAcquire, ConfirmedLeaseExpiresAt: time.Unix(2000, 0).UTC(),
		AtEntry:  server.DbContextBudget{State: "live", HasDeadline: true, RemainingNs: int64(time.Second)},
		AtReturn: server.DbContextBudget{State: "deadline", HasDeadline: true}, Acquire: proof.acquire,
		Transaction: server.DbTxObservationSnapshot{AcquireAttempts: 1}}
	return proof
}

// This causal test fails at its missing renewal_database assertion when only
// the serializer's diagnostic assignment is removed, while old fields survive.
func TestCompetitionSupervisorRenewalDiagnosticsMachineReportIncludesExactFacts(t *testing.T) {
	proof := competitionSupervisorDiagnosticFailure()
	content, err := MarshalCompetitionSupervisorReport("lease_lost", proof)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		State           string                                       `json:"state"`
		Error           string                                       `json:"error"`
		RenewalDatabase *CompetitionSupervisorRenewalDatabaseFailure `json:"renewal_database"`
	}
	if err := json.Unmarshal(content, &result); err != nil || result.State != "lease_lost" || result.Error != proof.Error() ||
		result.RenewalDatabase == nil || !reflect.DeepEqual(result.RenewalDatabase, proof.database) {
		t.Fatalf("renewal machine report omitted or changed its completed database facts: %s error=%v", content, err)
	}
	diagnostic, err := json.Marshal(result.RenewalDatabase)
	if err != nil || len(diagnostic) > 8*1024 {
		t.Fatal("fixed diagnostic shape lost its bounded representation", len(diagnostic), err)
	}
	for _, forbidden := range []string{"\"error\"", "\"cause\"", "\"sql\"", "\"arguments\"", "\"owner_id\"", "\"query\"", "\"retryable\"", "\"accepted\""} {
		if strings.Contains(string(diagnostic), forbidden) {
			t.Fatal("observational diagnostic gained payload or authority fields", forbidden)
		}
	}
}

// Non-renewal and healthy reports keep their original two-field wire shape.
func TestCompetitionSupervisorRenewalDiagnosticsPreserveLegacyReports(t *testing.T) {
	for _, check := range []struct {
		state string
		err   error
		want  string
	}{
		{state: "evaluating", want: `{"state":"evaluating"}`},
		{state: "lease_lost", err: context.DeadlineExceeded, want: `{"state":"lease_lost","error":"context deadline exceeded"}`},
		{state: "blocked", err: errors.New("synthetic unknown"), want: `{"state":"blocked","error":"synthetic unknown"}`},
	} {
		content, err := MarshalCompetitionSupervisorReport(check.state, check.err)
		if err != nil || string(content) != check.want {
			t.Fatalf("legacy report changed: got=%s want=%s error=%v", content, check.want, err)
		}
	}
}

// A branch's authentic diagnostic cannot describe an unknown joined/wrapped
// failure. No generic Unwrap traversal is performed by the projection.
func TestCompetitionSupervisorRenewalDiagnosticsRejectUnenclosedProof(t *testing.T) {
	proof := competitionSupervisorDiagnosticFailure()
	for _, err := range []error{
		nil, context.DeadlineExceeded, &proof,
		fmt.Errorf("synthetic outer wrapper: %w", proof),
		errors.Join(proof, context.DeadlineExceeded), errors.Join(proof, proof),
		competitionSupervisorUnknownDeadline{cause: proof},
		competitionSupervisorRenewError{cause: context.DeadlineExceeded},
		(*competitionSupervisorRenewError)(nil),
	} {
		if CompetitionSupervisorRenewalFailureDetails(err) != nil {
			t.Errorf("unenclosed error projected one branch as its complete observation: %T", err)
		}
	}
}

// Snapshot publication happens after the original capture returns and owns
// both layers of pool-return copying without modifying cause identity.
func TestCompetitionSupervisorRenewalDiagnosticsDetachReturnedSnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	confirmed := time.Now().Add(time.Minute)
	observation := newCompetitionSupervisorRenewObservation()
	observation.timing.Acquire.PoolReturn = &server.DbAcquirePoolSnapshot{Maximum: 8, Idle: 2}
	cause := errors.New("synthetic private cause")
	err := observation.captureContext(ctx, confirmed, func() {
		observation.callbackEntered, observation.callbacks = true, 2
		observation.step = competitionSupervisorRenewalLeaseUpdate
		panic(cause)
	})
	if !errors.Is(err, cause) || err.Error() != cause.Error() {
		t.Fatal("diagnostics changed the original error", err)
	}
	first := CompetitionSupervisorRenewalFailureDetails(err)
	if first == nil || first.Schema != 1 || first.Kind != "competition-supervisor-renewal-database/v1" ||
		first.DatabasePhase != server.DbOperationUnknown || first.Step != competitionSupervisorRenewalLeaseUpdate || first.CallbackEntries != 2 ||
		!first.ConfirmedLeaseExpiresAt.Equal(confirmed) || first.AtEntry.State != "live" || first.AtReturn.State != "live" ||
		!first.AtEntry.HasDeadline || !first.AtReturn.HasDeadline || first.AtReturn.AtUnixNano < first.AtEntry.AtUnixNano {
		t.Fatal("completed capture omitted its exact observation boundaries", first)
	}
	observation.timing.Acquire.PoolReturn.Idle = 4
	first.Acquire.PoolReturn.Idle = 6
	first.Transaction.CommitAttempts = 1
	second := CompetitionSupervisorRenewalFailureDetails(err)
	if second == nil || second.Acquire.PoolReturn.Idle != 2 || second.Transaction.CommitAttempts != 0 {
		t.Fatal("published diagnostic aliases mutable observation or consumer state", second)
	}
	content, marshalErr := json.Marshal(second)
	if marshalErr != nil || strings.Contains(string(content), cause.Error()) {
		t.Fatal("database diagnostic copied a private cause payload", marshalErr)
	}
}

// Publishing a failure adds no retry or time allowance. The prior acquisition
// proof still authorizes exactly one fresh call after its owned first deadline.
func TestCompetitionSupervisorRenewalDiagnosticsPreserveAcquireRetryCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorRenewalSettings()
		started, calls := time.Now(), 0
		err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(time.Minute), settings, func(ctx context.Context) error {
			calls++
			if calls == 1 {
				<-ctx.Done()
				proof := competitionSupervisorDiagnosticFailure()
				if _, err := MarshalCompetitionSupervisorReport("lease_lost", proof); err != nil {
					t.Fatal(err)
				}
				return proof
			}
			return nil
		})
		if err != nil || calls != 2 || time.Since(started) != settings.AttemptTimeout+settings.LockRetry.Interval {
			t.Fatal("diagnostic publication changed acquisition-only continuation", calls, time.Since(started), err)
		}
	})
}

// An admitted deadline, possible commit, or wholly unknown failure remains
// terminal even when all diagnostic fields can be serialized successfully.
func TestCompetitionSupervisorRenewalDiagnosticsDoNotRetryAdmissionOrCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, phase := range []server.DbOperationPhase{server.DbOperationUnknown, server.DbOperationBegin, server.DbOperationCallback, server.DbOperationCommit, server.DbOperationAcknowledged} {
			settings := defaultCompetitionSupervisorRenewalSettings()
			started, calls := time.Now(), 0
			err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(time.Minute), settings, func(ctx context.Context) error {
				calls++
				<-ctx.Done()
				proof := competitionSupervisorDiagnosticFailure()
				proof.phase, proof.read.AcquireSucceeded, proof.callbackEntered = phase, 1, true
				proof.database.DatabasePhase, proof.database.CallbackEntries = phase, 1
				proof.database.Transaction.AcquireAttempts, proof.database.Transaction.AcquireSucceeded = 1, 1
				proof.database.Transaction.CallbackEntered, proof.database.Transaction.CommitAttempts = 1, 1
				if competitionSupervisorUnadmittedDeadline(proof) {
					t.Fatal("diagnostic or ambiguous commit granted acquisition proof")
				}
				if _, err := MarshalCompetitionSupervisorReport("lease_lost", proof); err != nil {
					t.Fatal(err)
				}
				return proof
			})
			if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || time.Since(started) != settings.AttemptTimeout {
				t.Fatal("diagnostics broadened retry/allowance for admitted or unknown failure", phase, calls, err)
			}
		}
	})
}
