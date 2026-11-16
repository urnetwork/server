// The two incident classes reach terminal state through real store operations,
// then use the same audited post-close cycle instead of resetting failed rows.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Bind the actual retained event and refreshed approval digest after every
// mutation; a stale digest must never stand in for a new operator approval.
func sealCompetitionIncidentReevaluationFixture(t testing.TB, ctx context.Context,
	fixture *competitionReevaluationAuthorizationPgFixture, eventType string) {
	t.Helper()
	a := &fixture.request.Authorization
	fixture.runtime.IncidentClasses = []string{a.IncidentClass}
	runtimeSha256, err := model.CompetitionAutomationDigest(fixture.runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := BindCompetitionJobReevaluationRuntime(fixture.settings, fixture.runtime, runtimeSha256); err != nil {
		t.Fatal(err)
	}
	a.RuntimeQualificationSha256 = runtimeSha256
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT event_id,payload_sha256 FROM competition_job_event
		 WHERE job_id=$1 AND event_type=$2 ORDER BY event_id DESC LIMIT 1`, a.JobId, eventType).Scan(&a.IncidentEventId, &a.IncidentEventSha256))
	})
	a.FailureManifestSha256, a.RejectedArtifactPath = "", ""
	competitionPostCloseReevaluationRequest(t, fixture)
	if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, fixture.principal.Id,
		competitionFinalizationHoldArgs("synthetic-incident-finalization")); err != nil {
		t.Fatal(err)
	}
}

// A genuine submission classification remains in the public recovery receipt
// and private terminal snapshot; only its explicit operator class allows a retry.
func TestCompetitionReevaluationBackingServicePgPreservesOriginalSubmissionFailure(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		requireCompetitionPostCloseFixtureSchema(t, ctx)
		fixture := newCompetitionReevaluationAuthorizationPgFixtureWithTerminal(t, ctx, "backing-service", func(*Settings, *queuedJob) EvaluationOutcome {
			return EvaluationOutcome{Error: submissionError("run_unstable", "synthetic recovered PostgreSQL timeout")}
		}, true)
		a := &fixture.request.Authorization
		a.IncidentClass = model.CompetitionJobReevaluationBackingService
		a.IncidentAttempt = a.ExpectedAttemptCount
		content := []byte(syntheticCompetitionBackingServiceRecovery(t))
		digest := sha256.Sum256(content)
		a.BackingServiceEvidence = &model.CompetitionJobReevaluationBackingServiceEvidence{Schema: 1, Service: "postgresql",
			ArtifactPath:   "evidence/scorer-input/candidate-01-" + a.JobId.String()[:8] + "/stderr.log",
			ArtifactSha256: hex.EncodeToString(digest[:]), ArtifactBytes: int64(len(content))}
		path := filepath.Join(fixture.settings.ArtifactRoot, a.JobId.String(), fmt.Sprintf("attempt-%02d", a.IncidentAttempt), a.BackingServiceEvidence.ArtifactPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o400); err != nil {
			t.Fatal(err)
		}
		sealCompetitionIncidentReevaluationFixture(t, ctx, fixture, "failed")
		before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request)
		if err != nil || receipt == nil {
			t.Fatalf("bound backing-service approval refused: %+v %v", receipt, err)
		}
		if receipt.IncidentClass != model.CompetitionJobReevaluationBackingService || receipt.PreviousFailure.Kind != "submission" ||
			receipt.PreviousFailure.Code != "run_unstable" || model.ValidateCompetitionJobReevaluationCycle(&receipt.CompetitionJobReevaluationCycle) != nil {
			t.Fatal("recovery relabelled the old failure or omitted its typed approval")
		}
		requireCompetitionReevaluationOriginalEvidence(t, before, readCompetitionReevaluationPgSnapshot(ctx, fixture.job))
	})
}

// Hand-back events historically omit attempt. Resolve the last real claim at
// that event's ordered identity, not the next claim at the identical timestamp.
func TestCompetitionReevaluationHeartbeatPgBindsInterruptedClaimBeforeTerminal(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		requireCompetitionPostCloseFixtureSchema(t, ctx)
		interruptedAttempt := 0
		fixture := newCompetitionReevaluationAuthorizationPgFixtureWithArchiveEvidence(t, ctx, "heartbeat-budget", func(*Settings, *queuedJob) EvaluationOutcome {
			return EvaluationOutcome{Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "synthetic interrupted execution exhausted original budget")}
		}, true, false, func(f *competitionReevaluationAuthorizationPgFixture) {
			interruptedAttempt = f.job.AttemptCount
			if err := f.store.handBackWithFailure(ctx, "synthetic-reevaluation-worker", f.job.JobId, "heartbeat_failed",
				competitionHeartbeatFailure{Category: "deadline_exceeded", DetailSha256: strings.Repeat("c", 64)}); err != nil {
				t.Fatal(err)
			}
			var err error
			f.job, err = f.store.Claim(ctx, f.settings, "synthetic-reevaluation-worker", testWorkerImageDigest())
			if err != nil || f.job == nil {
				t.Fatalf("claim after actual heartbeat hand-back: %v", err)
			}
		})
		fixture.request.Authorization.IncidentClass = model.CompetitionJobReevaluationHeartbeatBudget
		fixture.request.Authorization.IncidentAttempt = interruptedAttempt
		sealCompetitionIncidentReevaluationFixture(t, ctx, fixture, "handed_back")
		before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		wrong := fixture.request
		wrong.Authorization.IncidentAttempt--
		var err error
		wrong.AuthorizationSha256, err = model.CompetitionAutomationDigest(wrong.Authorization)
		if err != nil {
			t.Fatal(err)
		}
		if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, wrong); err == nil || receipt != nil {
			t.Fatal("another claim borrowed the heartbeat event's authority")
		}
		if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request); err != nil || receipt == nil ||
			receipt.IncidentClass != model.CompetitionJobReevaluationHeartbeatBudget || receipt.PreviousFailure.Kind != "infrastructure" {
			t.Fatalf("actual interrupted claim did not qualify: %+v %v", receipt, err)
		}
		requireCompetitionReevaluationOriginalEvidence(t, before, readCompetitionReevaluationPgSnapshot(ctx, fixture.job))
	})
}
