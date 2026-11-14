// Same-job polling preserves the paid association, original hidden failure and
// publication embargo across the explicit operator-requested retry lifecycle.
package controller

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competitionagent"
	"github.com/urnetwork/server/v2026/model"
)

// The real file-store admission and initial poll have a nonempty fee receipt.
// New metadata never creates another paid association or hides the old failure.
func TestCompetitionOperatorPatchRetryApexKeepsFeeOriginalFailureAndPrivateFeedback(t *testing.T) {
	store, name, poll, now := newApexReevaluationFixtureWithFailure(t, true, EvaluationFailure{Kind: "infrastructure", Code: "executor_process_failed"})
	before, err := store.Get(name)
	if err != nil || before.FeeReceipt == "" {
		t.Fatal("synthetic paid association absent", err)
	}
	cycle := competitionOperatorPatchRetryCycleForTest(t)
	cycle.JobId, cycle.RoundId, cycle.PatchSha256, cycle.AuthorizedAt = poll.JobId, poll.RoundId, poll.PatchSha256, now
	cycle.Distributed.OriginalStartedAt, cycle.Distributed.OriginalDeadlineAt = now.Add(-time.Minute), now.Add(-time.Minute).Add(3*time.Hour)
	cycle.Distributed.OriginalRunId = competitionagent.RunId(poll.JobId.String(), 0, 1)
	cycle.Distributed.PatchRetry.PatchSha256 = poll.PatchSha256
	sealApexReevaluationCycle(t, &cycle)
	poll.Reevaluation = &cycle
	record, err := store.RecordPoll(name, poll, now)
	if err != nil || record.FeeReceipt != before.FeeReceipt || record.JobId != before.JobId || len(record.ReevaluationHistory) != 1 ||
		record.ReevaluationHistory[0].EvaluationFailure != *before.EvaluationFailure || record.ReevaluationHistory[0].ObservedAt != before.UpdatedAt {
		t.Fatal("operator retry changed original fee/admission/failure", err)
	}
	started := now.Add(time.Second)
	poll.State, poll.EvaluationStatus, poll.Reevaluation.StartedAt = "running", "running", &started
	if _, err := store.RecordPoll(name, poll, started); err != nil {
		t.Fatal(err)
	}
	poll.State, poll.EvaluationStatus, poll.Score = "completed", "completed", privateCompletedScore(privateScoreFeedbackFixture())
	record, err = store.RecordPoll(name, poll, started.Add(time.Minute))
	if err != nil || record.FeeReceipt != before.FeeReceipt || record.PrivateScore == nil || record.PrivateScore.Placeable || record.Published || record.Winner || record.Score != nil {
		t.Fatal("operator patch retry broadened publication or charged again", err)
	}
	reopened, err := NewApexAdapterFileStore(store.directory)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reopened.Get(name)
	if err != nil || !reflect.DeepEqual(saved, record) {
		t.Fatal("retry failure provenance changed after cache restart", err)
	}
	poll.State, poll.EvaluationStatus, poll.Score = "queued", "queued", nil
	if _, err := reopened.RecordPoll(name, poll, now.Add(time.Hour)); err == nil {
		t.Fatal("completed retry restarted through polling")
	}
}

// An older API intentionally omitted this not-yet-reviewed generic code. Only
// the authenticated exact new-class receipt may supply that missing summary.
func TestCompetitionOperatorPatchRetryApexCanDiscoverReceiptAfterOmittedLegacyFailure(t *testing.T) {
	store, err := NewApexAdapterFileStore(filepath.Join(t.TempDir(), "synthetic-omitted-patch-failure"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	round := RoundResult{RoundId: server.NewId(), DecisionMode: model.CompetitionDecisionModeV2, RequiredApiCapability: model.CompetitionAutomationCapability}
	name := "synthetic-omitted-patch-retry"
	preparePrivateScoreAssociation(t, store, name, round, now)
	jobId := server.NewId()
	patchSha256 := strings.Repeat("a", 64)
	if _, err := store.RecordAdmission(name, ScoreAcceptedResult{JobId: jobId, RoundId: round.RoundId, PatchSha256: patchSha256,
		State: "queued", StatusUrl: "/competition/v2/score/" + jobId.String(), DecisionMode: round.DecisionMode, RequiredApiCapability: round.RequiredApiCapability}, now); err != nil {
		t.Fatal(err)
	}
	poll := ScoreJobResult{JobId: jobId, RoundId: round.RoundId, PatchSha256: patchSha256, State: "completed", EvaluationStatus: "failed", DecisionMode: round.DecisionMode, RequiredApiCapability: round.RequiredApiCapability}
	before, err := store.RecordPoll(name, poll, now.Add(time.Second))
	if err != nil || !apexReevaluationRefreshEligible(*before) || before.EvaluationFailure != nil {
		t.Fatal("legacy missing code cannot perform bounded read-only receipt discovery", err)
	}
	cycle := competitionOperatorPatchRetryCycleForTest(t)
	cycle.JobId, cycle.RoundId, cycle.PatchSha256, cycle.AuthorizedAt = jobId, round.RoundId, patchSha256, now.Add(time.Minute)
	cycle.Distributed.OriginalStartedAt, cycle.Distributed.OriginalDeadlineAt = now, now.Add(3*time.Hour)
	cycle.Distributed.OriginalRunId = competitionagent.RunId(jobId.String(), 0, 1)
	cycle.Distributed.PatchRetry.PatchSha256 = patchSha256
	sealApexReevaluationCycle(t, &cycle)
	poll.State, poll.EvaluationStatus, poll.Reevaluation = "queued", "queued", &cycle
	record, err := store.RecordPoll(name, poll, cycle.AuthorizedAt)
	if err != nil || record.FeeReceipt != before.FeeReceipt || len(record.ReevaluationHistory) != 1 ||
		record.ReevaluationHistory[0].EvaluationFailure != cycle.PreviousFailure || record.ReevaluationHistory[0].ObservedAt != before.UpdatedAt {
		t.Fatal("typed receipt could not preserve legacy missing generic failure", err)
	}
	if apexReevaluationRefreshEligible(*record) {
		t.Fatal("consumed operator patch retry remained refresh eligible")
	}
}

// Mutable public proof pointers cannot change a retained receipt after polling
// or alter another cache association through a shared struct copy.
func TestCompetitionOperatorPatchRetryApexCloneOwnsNestedEvidence(t *testing.T) {
	cycle := competitionOperatorPatchRetryCycleForTest(t)
	copy := cloneApexReevaluationCycle(&cycle)
	copy.Distributed.PatchRetry.ApplicabilityEvidenceSha256 = strings.Repeat("9", 64)
	copy.Distributed.SourceLockSha256 = strings.Repeat("8", 64)
	if reflect.DeepEqual(copy, &cycle) || cycle.Distributed.PatchRetry.ApplicabilityEvidenceSha256 == copy.Distributed.PatchRetry.ApplicabilityEvidenceSha256 ||
		cycle.Distributed.SourceLockSha256 == copy.Distributed.SourceLockSha256 {
		t.Fatal("cloned retry receipt aliases nested original evidence")
	}
}
