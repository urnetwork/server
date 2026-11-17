// The new distributed marker is consumed by ordinary authenticated same-job
// polling, never a new admission, fee charge or score-publication capability.
package controller

import (
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server/competitionagent"
	"github.com/urnetwork/server/model"
)

// Cached original failure and paid association survive queue/run/completion
// and a file-store restart; old-cycle polling cannot resurrect another sample.
func TestCompetitionDistributedRecoveryApexPreservesFeeFailureAndPrivateFeedback(t *testing.T) {
	store, name, poll, now := newApexReevaluationFixtureWithFailure(t, true, EvaluationFailure{Kind: "infrastructure", Code: "evaluation_time_budget_exhausted"})
	before, err := store.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	cycle := poll.Reevaluation
	cycle.Schema, cycle.IncidentClass = 2, model.CompetitionJobReevaluationDistributedInfrastructure
	metadata := competitionDistributedRecoveryCycleForTest(t).Distributed
	metadata.OriginalStartedAt = now.Add(-4 * time.Hour)
	metadata.OriginalDeadlineAt = metadata.OriginalStartedAt.Add(3 * time.Hour)
	metadata.OriginalRunId = competitionagent.RunId(poll.JobId.String(), 0, 1)
	cycle.Distributed = metadata
	sealApexReevaluationCycle(t, cycle)
	record, err := store.RecordPoll(name, poll, now)
	if err != nil || record.FeeReceipt != before.FeeReceipt || record.JobId != before.JobId || len(record.ReevaluationHistory) != 1 || record.ReevaluationHistory[0].EvaluationFailure != *before.EvaluationFailure {
		t.Fatal("distributed recovery changed original fee/admission/failure history", err)
	}
	started := now.Add(time.Second)
	poll.State, poll.EvaluationStatus, poll.Reevaluation.StartedAt = "running", "running", &started
	if _, err := store.RecordPoll(name, poll, started); err != nil {
		t.Fatal(err)
	}
	poll.State, poll.EvaluationStatus, poll.Score = "completed", "completed", privateCompletedScore(privateScoreFeedbackFixture())
	record, err = store.RecordPoll(name, poll, started.Add(time.Minute))
	if err != nil || record.FeeReceipt != before.FeeReceipt || record.Score != nil || record.Published || record.Winner || record.PrivateScore == nil || record.PrivateScore.Placeable {
		t.Fatal("distributed recovery widened score publication or charged again", err)
	}
	reopened, err := NewApexAdapterFileStore(store.directory)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reopened.Get(name)
	if err != nil || !reflect.DeepEqual(saved, record) {
		t.Fatal("distributed recovery provenance changed across cache restart", err)
	}
	poll.State, poll.EvaluationStatus, poll.Score = "queued", "queued", nil
	if _, err := reopened.RecordPoll(name, poll, now.Add(time.Hour)); err == nil {
		t.Fatal("completed distributed recovery was reused as a fresh evaluation")
	}
}
