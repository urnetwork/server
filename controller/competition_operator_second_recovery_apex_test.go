// Real paid cache associations retain both failed observations and the full
// immutable prior receipt without gaining publication or another admission.
package controller

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competitionagent"
	"github.com/urnetwork/server/model"
)

// A cache can observe the first failed recovery or remain offline throughout it.
// The authenticated bounded receipt chain supports both histories consistently.
func newCompetitionSecondRecoveryApexFixture(t testing.TB, observedFirst bool) (*ApexAdapterFileStore, string, ScoreJobResult, time.Time) {
	t.Helper()
	store, name, poll, now := newApexReevaluationFixtureWithFailure(t, true, EvaluationFailure{Kind: "infrastructure", Code: "executor_process_failed"})
	first := competitionOperatorPatchRetryCycleForTest(t)
	first.JobId, first.RoundId, first.PatchSha256, first.AuthorizedAt = poll.JobId, poll.RoundId, poll.PatchSha256, now
	first.Distributed.PatchRetry.PatchSha256 = poll.PatchSha256
	first.Distributed.OriginalRunId = competitionagent.RunId(poll.JobId.String(), 0, 1)
	first.Distributed.OriginalStartedAt, first.Distributed.OriginalDeadlineAt = now.Add(-time.Minute), now.Add(-time.Minute).Add(3*time.Hour)
	sealApexReevaluationCycle(t, &first)
	started := now.Add(time.Second)
	if observedFirst {
		poll.Reevaluation = cloneApexReevaluationCycle(&first)
		if _, err := store.RecordPoll(name, poll, now); err != nil {
			t.Fatal(err)
		}
		poll.State, poll.EvaluationStatus, poll.Reevaluation.StartedAt = "running", "running", &started
		if _, err := store.RecordPoll(name, poll, started); err != nil {
			t.Fatal(err)
		}
		poll.State, poll.EvaluationStatus, poll.EvaluationFailure = "completed", "failed", &EvaluationFailure{Kind: "infrastructure", Code: "executor_process_failed"}
		if _, err := store.RecordPoll(name, poll, started.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	first.StartedAt = &started
	cycle := competitionRecoveryTestCopy(t, first)
	cycle.Schema, cycle.Cycle, cycle.StartedAt = 3, 2, nil
	cycle.AuthorizedAt = started.Add(time.Minute)
	cycle.Distributed.OriginalStartedAt, cycle.Distributed.OriginalDeadlineAt = started, started.Add(3*time.Hour)
	cycle.Distributed.OriginalRunId = competitionagent.RunId(poll.JobId.String(), 1, 2)
	cycle.PreviousTerminalSha256 = strings.Repeat("9", 64)
	cycle.SecondRecovery = &model.CompetitionOperatorSecondRecovery{Schema: 1, JobId: poll.JobId,
		PreviousReceiptSha256: first.ReceiptSha256, ApprovalEvidenceSha256: strings.Repeat("8", 64), PreviousRecovery: &first}
	sealApexReevaluationCycle(t, &cycle)
	poll.State, poll.EvaluationStatus, poll.EvaluationFailure, poll.Reevaluation = "queued", "queued", nil, &cycle
	return store, name, poll, cycle.AuthorizedAt
}

// Restart and normal private feedback cannot erase either prior failure,
// replace the paid association, publish a score, or reset a completed cycle.
func TestCompetitionSecondRecoveryApexRetainsBothFailuresFeeAndPrivateFeedback(t *testing.T) {
	for _, observedFirst := range []bool{false, true} {
		store, name, poll, now := newCompetitionSecondRecoveryApexFixture(t, observedFirst)
		before, err := store.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		record, err := store.RecordPoll(name, poll, now)
		if err != nil || record.FeeReceipt != before.FeeReceipt || record.JobId != before.JobId || len(record.ReevaluationHistory) != 2 ||
			record.ReevaluationHistory[0].Cycle != 0 || record.ReevaluationHistory[1].Cycle != 1 || record.Reevaluation.Cycle != 2 || record.Published || record.Winner {
			t.Fatal("second recovery lost original history/admission or broadened publication", observedFirst, err)
		}
		if observedFirst && record.ReevaluationHistory[0] != before.ReevaluationHistory[0] {
			t.Fatal("second receipt rewrote original cached observation")
		}
		history := append([]ApexReevaluationObservation(nil), record.ReevaluationHistory...)
		started := now.Add(time.Second)
		poll.State, poll.EvaluationStatus, poll.Reevaluation.StartedAt = "running", "running", &started
		if _, err := store.RecordPoll(name, poll, started); err != nil {
			t.Fatal(err)
		}
		poll.State, poll.EvaluationStatus, poll.Score = "completed", "completed", privateCompletedScore(privateScoreFeedbackFixture())
		record, err = store.RecordPoll(name, poll, started.Add(time.Minute))
		if err != nil || record.FeeReceipt != before.FeeReceipt || record.PrivateScore == nil || record.PrivateScore.Placeable || record.Score != nil || record.Published || record.Winner ||
			!reflect.DeepEqual(history, record.ReevaluationHistory) {
			t.Fatal("second recovery changed embargo, fee or history", err)
		}
		reopened, err := NewApexAdapterFileStore(store.directory)
		if err != nil {
			t.Fatal(err)
		}
		saved, err := reopened.Get(name)
		if err != nil || !reflect.DeepEqual(record, saved) {
			t.Fatal("second recovery did not survive cache restart", err)
		}
		poll.State, poll.EvaluationStatus, poll.Score = "queued", "queued", nil
		if _, err := reopened.RecordPoll(name, poll, now.Add(time.Hour)); err == nil {
			t.Fatal("completed cycle two restarted through polling")
		}
	}
}

// Resealed wrong-job/prior authority, private scoring and stale first receipts
// cannot alter a durable second-cycle cache or its fee/history.
func TestCompetitionSecondRecoveryApexRejectsTamperedChainAndStalePollAtomically(t *testing.T) {
	store, name, poll, now := newCompetitionSecondRecoveryApexFixture(t, true)
	if _, err := store.RecordPoll(name, poll, now); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ScoreJobResult){
		func(p *ScoreJobResult) {
			p.Reevaluation = cloneApexReevaluationCycle(p.Reevaluation.SecondRecovery.PreviousRecovery)
		},
		func(p *ScoreJobResult) { p.Reevaluation.SecondRecovery.PreviousReceiptSha256 = strings.Repeat("f", 64) },
		func(p *ScoreJobResult) { p.Reevaluation.SecondRecovery.JobId = server.NewId() },
		func(p *ScoreJobResult) { p.Reevaluation.Cycle = 3 },
		func(p *ScoreJobResult) { p.Reevaluation = nil },
	} {
		changed := competitionRecoveryTestCopy(t, poll)
		change(&changed)
		if changed.Reevaluation != nil {
			sealApexReevaluationCycle(t, changed.Reevaluation)
		}
		if _, err := store.RecordPoll(name, changed, now.Add(time.Minute)); err == nil {
			t.Fatal("tampered or stale second-cycle poll accepted")
		}
		after, err := store.Get(name)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("rejected poll changed durable paid history", err)
		}
	}
}

// Only the exact approved job's validated failed first operator retry may use
// bounded read-only discovery; neither successful cycles nor other jobs do.
func TestCompetitionSecondRecoveryApexDiscoveryIsExactFailedFirstCycleOnly(t *testing.T) {
	store, name, _, _ := newCompetitionSecondRecoveryApexFixture(t, true)
	record, err := store.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	approvedJobId := record.JobId
	if !apexReevaluationRefreshEligibleForJob(*record, approvedJobId) {
		t.Fatal("approved failed first cycle cannot discover its successor")
	}
	for _, change := range []func(*ApexAdapterRecord){
		func(r *ApexAdapterRecord) { r.JobId = server.NewId() },
		func(r *ApexAdapterRecord) { r.EvaluationStatus = "completed" },
		func(r *ApexAdapterRecord) { r.EvaluationStatus = "running" },
		func(r *ApexAdapterRecord) { r.PrivateScore = privateScoreFeedbackFixture() },
		func(r *ApexAdapterRecord) { r.Published = true },
		func(r *ApexAdapterRecord) { r.Reevaluation.Schema = 1 },
		func(r *ApexAdapterRecord) {
			r.Reevaluation.IncidentClass = model.CompetitionJobReevaluationDistributedInfrastructure
		},
		func(r *ApexAdapterRecord) { r.Reevaluation.ReceiptSha256 = strings.Repeat("f", 64) },
	} {
		changed := competitionRecoveryTestCopy(t, *record)
		change(&changed)
		if apexReevaluationRefreshEligibleForJob(changed, approvedJobId) {
			t.Fatal("discovery widened beyond the approved failed first cycle")
		}
	}
	if apexReevaluationRefreshEligibleForJob(*record, server.NewId()) {
		t.Fatal("another approval discovered this job's second recovery")
	}
}

// A late alias learns both receipts/failures while retaining its own original
// association and fee; a successor does not become a new paid admission.
func TestCompetitionSecondRecoveryApexLateAliasInheritsBoundedHistory(t *testing.T) {
	store, name, poll, now := newCompetitionSecondRecoveryApexFixture(t, true)
	if _, err := store.RecordPoll(name, poll, now); err != nil {
		t.Fatal(err)
	}
	alias := "synthetic-second-recovery-alias"
	round := RoundResult{RoundId: poll.RoundId, DecisionMode: poll.DecisionMode, RequiredApiCapability: poll.RequiredApiCapability}
	preparePrivateScoreAssociation(t, store, alias, round, now)
	accepted := ScoreAcceptedResult{JobId: poll.JobId, RoundId: poll.RoundId, PatchSha256: poll.PatchSha256, State: "completed", CacheHit: true,
		StatusUrl: "/competition/v2/score/" + poll.JobId.String(), DecisionMode: poll.DecisionMode, RequiredApiCapability: poll.RequiredApiCapability}
	if _, err := store.RecordAdmission(alias, accepted, now); err != nil {
		t.Fatal(err)
	}
	record, err := store.Get(alias)
	if err != nil || record.Reevaluation == nil || record.Reevaluation.Cycle != 2 || len(record.ReevaluationHistory) != 2 ||
		record.FeeReceipt != "synthetic-fee-"+alias || record.Published || record.Winner {
		t.Fatal("late alias lost bounded history or paid association", err)
	}
	copy := cloneApexReevaluationCycle(record.Reevaluation)
	copy.SecondRecovery.PreviousRecovery.Distributed.PatchRetry.ApplicabilityEvidenceSha256 = strings.Repeat("f", 64)
	if copy.SecondRecovery.PreviousRecovery.Distributed.PatchRetry.ApplicabilityEvidenceSha256 == record.Reevaluation.SecondRecovery.PreviousRecovery.Distributed.PatchRetry.ApplicabilityEvidenceSha256 {
		t.Fatal("cloned successor aliases the immutable predecessor")
	}
}
