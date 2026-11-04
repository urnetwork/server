// Finalization status is prospective. It never asserts missing readiness,
// discloses private evidence or hides a previously authorized published seal.
package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
)

// The exact scheduler capability adds neither operator nor settlement authority.
func TestCompetitionFinalizationHoldRequiresSchedulerRole(t *testing.T) {
	if competitionHoldRole("finalization") != competitionSchedulerRole || competitionHoldRole("all") != "" {
		t.Fatal("finalization hold accepted an unscoped or unrelated capability")
	}
}

// Actual readiness and admission checks remain authoritative while only the
// new optional finalization projection changes to held.
func TestCompetitionFinalizationHoldPreservesReadyIntakeAndGrading(t *testing.T) {
	_, store, service := newAutomationStagingApiFixture(t)
	store.fakeStore.holdStatus.Finalization = true
	ready, err := service.Ready(context.Background())
	if err != nil || !ready.Ready || ready.RuntimeStatus == nil || !ready.RuntimeStatus.IntakeReady || !ready.RuntimeStatus.GradingReady || ready.RuntimeStatus.PublicationHeld || !ready.RuntimeStatus.FinalizationHeld {
		t.Fatal("finalization hold altered intake or grading readiness", ready, err)
	}
	info, err := service.Info(context.Background())
	if err != nil || info.RuntimeStatus == nil || !info.RuntimeStatus.FinalizationHeld || !info.RuntimeStatus.GradingReady {
		t.Fatal("finalization hold was omitted from runtime discovery", info, err)
	}
	accepted, status, err := service.SubmitV2(context.Background(), ScoreArgs{RoundId: store.stagingRound.RoundId, Patch: testPatch("synthetic-finalization-intake")}, &Principal{Id: "synthetic-submitter", Role: "submitter"})
	if err != nil || status != http.StatusAccepted || accepted.State != "queued" || store.enqueueCalls != 1 {
		t.Fatal("finalization hold blocked unchanged submission admission", accepted, status, err)
	}
}

// A completed credential owner's private numeric feedback keeps the original
// public embargo; the hold does not upgrade readiness or expose hidden markers.
func TestCompetitionFinalizationHoldPreservesPrivateCompletedFeedback(t *testing.T) {
	settings := automationApiSettings()
	policy, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: server.NewId(), State: "succeeded", Score: privateScoreFeedbackFixture()},
		Round: roundRecord{RoundResult: RoundResult{RevealAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}, PolicyJson: policy}}
	job.Round.RoundId = job.RoundId
	store := &fakeStore{getJob: job, holdStatus: competitionAutomationHoldStatus{Finalization: true}}
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	result, status, evalError := service.GetScoreV2(context.Background(), job.JobId, &Principal{Id: "synthetic-credential-owner", Role: "submitter"})
	if evalError != nil || status != http.StatusOK || result.State != "completed" || result.Score == nil || !privateScoreProjectionMatches(result.Score) || job.Round.AutomationPublicationAt != nil {
		t.Fatal("finalization hold changed private completed-score feedback", result, status, evalError)
	}
}

// A seal's earlier authorization remains visible even when the hold is active;
// it is not an evaluation hold and cannot erase public historical score detail.
func TestCompetitionFinalizationHoldDoesNotHidePublishedHistory(t *testing.T) {
	_, store, service := newAutomationStagingApiFixture(t)
	store.fakeStore.holdStatus.Finalization = true
	job := *store.enqueueJob
	published := server.NowUtc().Add(-time.Minute)
	job.State, job.Score = "succeeded", privateScoreFeedbackFixture()
	job.Round.AutomationPublicationAt = &published
	store.fakeStore.getJob = &job
	result, status, err := service.GetScoreV2(context.Background(), job.JobId, &Principal{Id: "synthetic-submitter", Role: "submitter"})
	if err != nil || status != http.StatusOK || result.Score == nil || result.Score.Diagnostics["seed"] == nil {
		t.Fatal("prospective finalization hold hid a previously authorized published score", result, status, err)
	}
}

// Omission clears the new gauge just like the original independent capabilities.
func TestCompetitionFinalizationHoldMetricClearsOnRelease(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	competitionAutomationMetricSnapshot{activeHoldKinds: []string{"finalization"}}.publish(now)
	if testutil.ToFloat64(competitionAutomationHoldActive.WithLabelValues("finalization")) != 1 || testutil.ToFloat64(competitionAutomationHoldActive.WithLabelValues("evaluation")) != 0 {
		t.Fatal("finalization metric altered another held capability")
	}
	competitionAutomationMetricSnapshot{}.publish(now)
	if testutil.ToFloat64(competitionAutomationHoldActive.WithLabelValues("finalization")) != 0 {
		t.Fatal("released finalization left a stale active gauge")
	}
	if !strings.Contains(ErrFinalizationHeld.Error(), "finalization") {
		t.Fatal("ordinary finalization polling lost its typed capability identity")
	}
}
