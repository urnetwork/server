// Recovery polling regressions retain original failure history, authenticated
// scope, cache associations, bounded owner refreshes and publication embargoes.
package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The receipt is independently reconstructible; no signing secret is invented.
func sealApexReevaluationCycle(t testing.TB, cycle *model.CompetitionJobReevaluationCycle) {
	t.Helper()
	digest, err := model.CompetitionJobReevaluationCycleDigest(*cycle)
	if err != nil {
		t.Fatal(err)
	}
	cycle.ReceiptSha256 = digest
}

// Historical callers retain the original cached infrastructure fixture.
func newApexReevaluationFixture(t testing.TB, v2 bool) (*ApexAdapterFileStore, string, ScoreJobResult, time.Time) {
	t.Helper()
	return newApexReevaluationFixtureWithFailure(t, v2, EvaluationFailure{Kind: "infrastructure", Code: "artifact_archive_failed"})
}

// Public admission/polling records the chosen synthetic original failure;
// another test never rewrites an already cached terminal classification.
func newApexReevaluationFixtureWithFailure(t testing.TB, v2 bool, originalFailure EvaluationFailure) (*ApexAdapterFileStore, string, ScoreJobResult, time.Time) {
	t.Helper()
	store, err := NewApexAdapterFileStore(filepath.Join(t.TempDir(), "synthetic-apex-recovery"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	round := RoundResult{RoundId: server.NewId()}
	statusPrefix := "/competition/score/"
	if v2 {
		round.DecisionMode, round.RequiredApiCapability = model.CompetitionDecisionModeV2, model.CompetitionAutomationCapability
		statusPrefix = "/competition/v2/score/"
	}
	name := "synthetic-recovery-miner"
	preparePrivateScoreAssociation(t, store, name, round, now)
	job := ScoreJobResult{JobId: server.NewId(), RoundId: round.RoundId, PatchSha256: strings.Repeat("a", 64),
		State: "completed", EvaluationStatus: "failed", EvaluationFailure: &originalFailure,
		DecisionMode: round.DecisionMode, RequiredApiCapability: round.RequiredApiCapability}
	if _, err := store.RecordAdmission(name, ScoreAcceptedResult{JobId: job.JobId, RoundId: job.RoundId, PatchSha256: job.PatchSha256,
		State: "queued", StatusUrl: statusPrefix + job.JobId.String(), DecisionMode: job.DecisionMode, RequiredApiCapability: job.RequiredApiCapability}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordPoll(name, job, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	cycle := &model.CompetitionJobReevaluationCycle{
		Schema: 1, Cycle: 1, Environment: "synthetic-env", CompetitionId: "synthetic-competition", SeasonId: "synthetic-season",
		JobId: job.JobId, RoundId: job.RoundId, PatchSha256: job.PatchSha256,
		RoundPolicySha256: strings.Repeat("1", 64), BaselineSha256: strings.Repeat("2", 64),
		ReceiptSha256: strings.Repeat("3", 64), AuthorizationSha256: strings.Repeat("4", 64), EvidenceSha256: strings.Repeat("5", 64),
		PreviousTerminalSha256: strings.Repeat("6", 64), PreviousFailure: *job.EvaluationFailure,
		RuntimeQualificationSha256: strings.Repeat("7", 64), FixedSourceCommit: strings.Repeat("b", 40),
		FixedWorkerSha256: strings.Repeat("c", 64), FixedWorkerImageDigest: "sha256:" + strings.Repeat("d", 64),
		ApiImageDigest: "sha256:" + strings.Repeat("e", 64), AuthorizedAt: now, ExecutionTimeoutSeconds: 3 * 60 * 60,
	}
	if originalFailure.Kind == "submission" && originalFailure.Code == "run_unstable" {
		cycle.IncidentClass = model.CompetitionJobReevaluationBackingService
	}
	sealApexReevaluationCycle(t, cycle)
	job.State, job.EvaluationStatus, job.EvaluationFailure, job.Reevaluation = "queued", "queued", nil, cycle
	return store, name, job, now
}

// A same-job recovery moves through ordinary lifecycle validation, keeps the old
// failure and fee, and never publishes private completed-score feedback.
func TestCompetitionReevaluationApexPreservesFailureThroughCompletedCycle(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		store, name, poll, now := newApexReevaluationFixture(t, v2)
		before, err := store.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		record, err := store.RecordPoll(name, poll, now)
		if err != nil || record.State != "queued" || record.EvaluationFailure != nil || len(record.ReevaluationHistory) != 1 ||
			record.ReevaluationHistory[0].EvaluationFailure != *before.EvaluationFailure || record.JobId != before.JobId ||
			record.FeeReceipt != before.FeeReceipt || record.Reevaluation.ReceiptSha256 != poll.Reevaluation.ReceiptSha256 {
			t.Fatalf("v2=%t authenticated recovery lost identity/history: %#v %v", v2, record, err)
		}
		previous := record.ReevaluationHistory[0]
		startedAt := now.Add(time.Second)
		poll.Reevaluation.StartedAt, poll.State, poll.EvaluationStatus = &startedAt, "running", "running"
		if _, err := store.RecordPoll(name, poll, startedAt); err != nil {
			t.Fatal(err)
		}
		poll.State, poll.EvaluationStatus = "completed", "completed"
		poll.Score = privateCompletedScore(privateScoreFeedbackFixture())
		record, err = store.RecordPoll(name, poll, startedAt.Add(time.Second))
		if err != nil || record.PrivateScore == nil || record.Score != nil || record.EvalError != nil || record.Published || record.Winner ||
			record.PrivateScore.Placeable || record.ReevaluationHistory[0] != previous || record.FeeReceipt != before.FeeReceipt {
			t.Fatalf("v2=%t completed recovery broadened publication or erased failure: %#v %v", v2, record, err)
		}
		reopened, err := NewApexAdapterFileStore(store.directory)
		if err != nil {
			t.Fatal(err)
		}
		saved, err := reopened.Get(name)
		if err != nil || !reflect.DeepEqual(saved, record) {
			t.Fatalf("v2=%t recovery history did not survive restart: %#v %v", v2, saved, err)
		}
		poll.State, poll.EvaluationStatus, poll.Score = "queued", "queued", nil
		if _, err := reopened.RecordPoll(name, poll, now.Add(time.Hour)); err == nil {
			t.Fatal("completed recovery was reused for another sampling opportunity")
		}
	}
}

// Receipt checks precede every state mutation, including wrong-scope receipts
// whose canonical digests are internally consistent.
func TestCompetitionReevaluationApexRejectsUnboundCycleMarkers(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*ScoreJobResult)
		reseal bool
	}{
		{name: "unsigned", mutate: func(job *ScoreJobResult) { job.Reevaluation.ReceiptSha256 = "" }},
		{name: "wrong receipt", mutate: func(job *ScoreJobResult) { job.Reevaluation.ReceiptSha256 = strings.Repeat("f", 64) }},
		{name: "wrong job", mutate: func(job *ScoreJobResult) { job.Reevaluation.JobId = server.NewId() }, reseal: true},
		{name: "wrong round", mutate: func(job *ScoreJobResult) { job.Reevaluation.RoundId = server.NewId() }, reseal: true},
		{name: "wrong patch", mutate: func(job *ScoreJobResult) { job.Reevaluation.PatchSha256 = strings.Repeat("f", 64) }, reseal: true},
		{name: "wrong competition", mutate: func(job *ScoreJobResult) { job.Reevaluation.CompetitionId = "synthetic-other-competition" }, reseal: true},
		{name: "wrong prior failure", mutate: func(job *ScoreJobResult) { job.Reevaluation.PreviousFailure.Code = "evaluation_time_budget_exhausted" }, reseal: true},
		{name: "submission failure", mutate: func(job *ScoreJobResult) {
			job.Reevaluation.PreviousFailure.Kind, job.Reevaluation.PreviousFailure.Code = "submission", "run_unstable"
		}, reseal: true},
		{name: "extra cycle", mutate: func(job *ScoreJobResult) { job.Reevaluation.Cycle = 2 }, reseal: true},
		{name: "expanded budget", mutate: func(job *ScoreJobResult) { job.Reevaluation.ExecutionTimeoutSeconds++ }, reseal: true},
		{name: "missing qualification", mutate: func(job *ScoreJobResult) { job.Reevaluation.RuntimeQualificationSha256 = "" }, reseal: true},
		{name: "staging", mutate: func(job *ScoreJobResult) { job.Staging = true }},
		{name: "untyped state", mutate: func(job *ScoreJobResult) { job.EvaluationStatus = "" }},
		{name: "unclaimed running", mutate: func(job *ScoreJobResult) { job.State, job.EvaluationStatus = "running", "running" }},
		{name: "no marker", mutate: func(job *ScoreJobResult) { job.Reevaluation = nil }},
	} {
		store, name, poll, now := newApexReevaluationFixture(t, true)
		before, err := store.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		testCase.mutate(&poll)
		if testCase.reseal {
			sealApexReevaluationCycle(t, poll.Reevaluation)
		}
		if _, err := store.RecordPoll(name, poll, now); err == nil {
			t.Errorf("%s: invalid recovery marker was accepted", testCase.name)
		}
		after, err := store.Get(name)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("%s: rejected marker mutated durable cached failure: %#v %v", testCase.name, after, err)
		}
	}
}

// Once pinned, every immutable receipt field and the first-claim timestamp resist
// replay; a receipt from a superseded cycle cannot erase stronger observations.
func TestCompetitionReevaluationApexRejectsReceiptReplayAndStaleCycle(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*ScoreJobResult)
		reseal bool
	}{
		{name: "old cycle", mutate: func(job *ScoreJobResult) { job.Reevaluation = nil }},
		{name: "different authorization", mutate: func(job *ScoreJobResult) { job.Reevaluation.AuthorizationSha256 = strings.Repeat("f", 64) }, reseal: true},
		{name: "different baseline", mutate: func(job *ScoreJobResult) { job.Reevaluation.BaselineSha256 = strings.Repeat("f", 64) }, reseal: true},
		{name: "different environment", mutate: func(job *ScoreJobResult) { job.Reevaluation.Environment = "synthetic-other-env" }, reseal: true},
		{name: "different season", mutate: func(job *ScoreJobResult) { job.Reevaluation.SeasonId = "synthetic-other-season" }, reseal: true},
		{name: "different fixed source", mutate: func(job *ScoreJobResult) { job.Reevaluation.FixedSourceCommit = strings.Repeat("f", 40) }, reseal: true},
		{name: "lost first claim", mutate: func(job *ScoreJobResult) { job.Reevaluation.StartedAt = nil }},
		{name: "changed first claim", mutate: func(job *ScoreJobResult) {
			at := job.Reevaluation.StartedAt.Add(time.Second)
			job.Reevaluation.StartedAt = &at
		}},
	} {
		store, name, poll, now := newApexReevaluationFixture(t, false)
		if _, err := store.RecordPoll(name, poll, now); err != nil {
			t.Fatal(err)
		}
		startedAt := now.Add(time.Second)
		poll.State, poll.EvaluationStatus, poll.Reevaluation.StartedAt = "running", "running", &startedAt
		if _, err := store.RecordPoll(name, poll, startedAt); err != nil {
			t.Fatal(err)
		}
		before, err := store.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		testCase.mutate(&poll)
		if testCase.reseal {
			sealApexReevaluationCycle(t, poll.Reevaluation)
		}
		if _, err := store.RecordPoll(name, poll, now.Add(time.Minute)); err == nil {
			t.Errorf("%s: stale or conflicting cycle observation was accepted", testCase.name)
		}
		after, err := store.Get(name)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("%s: rejected replay changed durable state: %#v %v", testCase.name, after, err)
		}
	}
}

// A genuine failure inside the approved cycle is still terminal, not an
// invitation to recycle the same receipt and continue until a passing score.
func TestCompetitionReevaluationApexKeepsNewCycleFailureTerminal(t *testing.T) {
	store, name, poll, now := newApexReevaluationFixture(t, true)
	startedAt := now.Add(time.Second)
	poll.State, poll.EvaluationStatus, poll.Reevaluation.StartedAt = "completed", "failed", &startedAt
	poll.EvaluationFailure = &EvaluationFailure{Kind: "submission", Code: "run_unstable"}
	record, err := store.RecordPoll(name, poll, startedAt)
	if err != nil || record.EvaluationFailure.Code != "run_unstable" || len(record.ReevaluationHistory) != 1 {
		t.Fatalf("approved cycle did not preserve its genuine failure: %#v %v", record, err)
	}
	poll.State, poll.EvaluationStatus, poll.EvaluationFailure = "queued", "queued", nil
	if _, err := store.RecordPoll(name, poll, now.Add(time.Minute)); err == nil {
		t.Fatal("same-cycle receipt reset a genuine terminal failure")
	}
}

// Existing and late canonical cache aliases share current cycle evidence while
// their own fee, input digest and admission identities remain independent.
func TestCompetitionReevaluationApexUpdatesCanonicalCacheAliases(t *testing.T) {
	store, name, poll, now := newApexReevaluationFixture(t, true)
	round := RoundResult{RoundId: poll.RoundId, DecisionMode: poll.DecisionMode, RequiredApiCapability: poll.RequiredApiCapability}
	accepted := ScoreAcceptedResult{JobId: poll.JobId, RoundId: poll.RoundId, PatchSha256: poll.PatchSha256,
		State: "completed", CacheHit: true, StatusUrl: "/competition/v2/score/" + poll.JobId.String(),
		DecisionMode: poll.DecisionMode, RequiredApiCapability: poll.RequiredApiCapability}
	alias := "synthetic-recovery-alias"
	preparePrivateScoreAssociation(t, store, alias, round, now)
	if _, err := store.RecordAdmission(alias, accepted, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordPoll(name, poll, now); err != nil {
		t.Fatal(err)
	}
	late := "synthetic-late-recovery-alias"
	preparePrivateScoreAssociation(t, store, late, round, now)
	if _, err := store.RecordAdmission(late, accepted, now); err != nil {
		t.Fatal(err)
	}
	for _, submissionId := range []string{name, alias, late} {
		record, err := store.Get(submissionId)
		if err != nil || record.State != "queued" || record.Reevaluation == nil ||
			record.Reevaluation.ReceiptSha256 != poll.Reevaluation.ReceiptSha256 || len(record.ReevaluationHistory) != 1 ||
			record.ReevaluationHistory[0].EvaluationFailure.Code != "artifact_archive_failed" || record.Published ||
			record.FeeReceipt != "synthetic-fee-"+submissionId {
			t.Fatalf("cache alias %s lost its cycle/history or own admission identity: %#v %v", submissionId, record, err)
		}
	}
}

// The authenticated owner lane discovers a cycle from an existing failed cache,
// reserves one shared job backoff before HTTP, and persists it across restart.
func TestCompetitionReevaluationApexRefreshDiscoversCycleAfterRestart(t *testing.T) {
	store, name, poll, now := newApexReevaluationFixture(t, true)
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		attempt := calls.Add(1)
		if request.Header.Get("Authorization") != "Bearer synthetic-recovery-owner" || request.URL.Path != "/competition/v2/score/"+poll.JobId.String() {
			t.Error("recovery probe lost owner authorization or immutable status URL")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeCompetitionJson(w, http.StatusOK, poll)
	}))
	defer api.Close()
	options := ApexAdapterOptions{HttpClient: api.Client(), MaxAttempts: 1, Now: func() time.Time { return now },
		V2CompetitionId: "synthetic-competition", PrivateScoreRefreshEvery: time.Minute}
	adapter, err := NewApexAdapter(api.URL, "synthetic-recovery-owner", store, &apexConformanceFeeCollector{}, options)
	if err != nil {
		t.Fatal(err)
	}
	if record, err := adapter.PollNext(context.Background()); err == nil || record != nil || calls.Load() != 1 {
		t.Fatalf("failed recovery probe did not reserve one request: %#v %v calls=%d", record, err, calls.Load())
	}
	reopened, err := NewApexAdapterFileStore(store.directory)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err = NewApexAdapter(api.URL, "synthetic-recovery-owner", reopened, &apexConformanceFeeCollector{}, options)
	if err != nil {
		t.Fatal(err)
	}
	if record, err := adapter.PollNext(context.Background()); err != nil || record != nil || calls.Load() != 1 {
		t.Fatalf("restart bypassed the recovery probe backoff: %#v %v calls=%d", record, err, calls.Load())
	}
	now = now.Add(time.Minute)
	record, err := adapter.PollNext(context.Background())
	if err != nil || record == nil || record.SubmissionId != name || record.State != "queued" ||
		record.Reevaluation == nil || len(record.ReevaluationHistory) != 1 || record.Published || calls.Load() != 2 {
		t.Fatalf("bounded owner probe did not discover the authorized cycle: %#v %v calls=%d", record, err, calls.Load())
	}
}

// Cursor work is bounded even with many old failures; an unchanged failed
// response never becomes a new cycle, feedback or publication by implication.
func TestCompetitionReevaluationApexRefreshHasBoundedCursorAndNoImplicitRetry(t *testing.T) {
	store, _, poll, now := newApexReevaluationFixture(t, false)
	store.reevaluationSettings.ScanLimit = 1
	if err := store.update(func(state *apexAdapterState) error {
		original := state.Records[0]
		for i := 0; i < 3; i++ {
			copy := original
			copy.Sequence = uint64(i + 2)
			copy.SubmissionId = []string{"synthetic-bounded-a", "synthetic-bounded-b", "synthetic-bounded-c"}[i]
			copy.JobId = server.NewId()
			copy.StatusUrl = "/competition/score/" + copy.JobId.String()
			state.Records = append(state.Records, copy)
		}
		state.NextSequence = 4
		state.ReevaluationRefreshNext = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	first, tried, err := store.reserveApexReevaluationRefresh(now, time.Minute)
	if err != nil || !tried || first == nil {
		t.Fatalf("bounded scan failed first admitted job: %#v %v", first, err)
	}
	if err := store.read(func(state *apexAdapterState) error {
		if state.ReevaluationRefreshCursor != 1 || len(state.ReevaluationRefreshAt) != 1 {
			t.Fatalf("one-record scan exceeded its examination budget: %#v", state)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	failed := poll
	failed.Reevaluation, failed.State, failed.EvaluationStatus = nil, "completed", "failed"
	failed.EvaluationFailure = &EvaluationFailure{Kind: "infrastructure", Code: "artifact_archive_failed"}
	record, err := store.RecordPoll(first.SubmissionId, failed, now)
	if err != nil || record.Reevaluation != nil || len(record.ReevaluationHistory) != 0 || record.EvaluationStatus != "failed" || record.PrivateScore != nil || record.Published {
		t.Fatalf("unchanged failed owner poll invented recovery: %#v %v", record, err)
	}
}

// Ordinary FIFO must remain runnable while a due recovery owner probe exists.
func TestCompetitionReevaluationApexRefreshAlternatesWithOrdinaryFifo(t *testing.T) {
	store, _, _, now := newApexReevaluationFixture(t, false)
	if err := store.update(func(state *apexAdapterState) error {
		active := state.Records[0]
		active.Sequence, active.SubmissionId, active.JobId = 2, "synthetic-active-fifo", server.NewId()
		active.State, active.EvaluationStatus, active.EvaluationFailure = "running", "running", nil
		active.StatusUrl = "/competition/score/" + active.JobId.String()
		state.Records = append(state.Records, active)
		state.NextSequence = 2
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	first, err := store.nextApexReevaluationAwarePoll(now, time.Minute)
	if err != nil || first == nil || first.SubmissionId != "synthetic-active-fifo" {
		t.Fatalf("ordinary FIFO lost its initial turn: %#v %v", first, err)
	}
	second, err := store.nextApexReevaluationAwarePoll(now, time.Minute)
	if err != nil || second == nil || second.SubmissionId == "synthetic-active-fifo" {
		t.Fatalf("due recovery lane was starved: %#v %v", second, err)
	}
	third, err := store.nextApexReevaluationAwarePoll(now, time.Minute)
	if err != nil || third == nil || third.SubmissionId != "synthetic-active-fifo" {
		t.Fatalf("recovery lane blocked ordinary FIFO: %#v %v", third, err)
	}
}

// Zero-cycle compatibility omits metadata; history cannot be fabricated or
// detached from the single authenticated receipt in a private state update.
func TestCompetitionReevaluationApexRejectsHistoryTamperingAndKeepsZeroCycleWire(t *testing.T) {
	store, name, poll, now := newApexReevaluationFixture(t, false)
	legacy := poll
	legacy.Reevaluation = nil
	encoded, err := json.Marshal(legacy)
	if err != nil || strings.Contains(string(encoded), "reevaluation") {
		t.Fatalf("original polling wire acquired a fabricated recovery marker: %s %v", encoded, err)
	}
	if _, err := store.RecordPoll(name, poll, now); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.update(func(state *apexAdapterState) error {
		state.Records[0].ReevaluationHistory[0].EvaluationFailure.Code = "run_unstable"
		return nil
	}); err == nil {
		t.Fatal("private cache accepted a rewritten prior failure")
	}
	after, err := store.Get(name)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected history tampering changed the saved record: %#v %v", after, err)
	}
}

// A conflict in the last alias cannot leave an earlier alias partly advanced,
// even for an in-memory caller outside the durable store's rollback boundary.
func TestCompetitionReevaluationApexAliasConflictRollsBackEveryObservation(t *testing.T) {
	store, name, poll, now := newApexReevaluationFixture(t, false)
	previous, err := store.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.RecordPoll(name, poll, now)
	if err != nil {
		t.Fatal(err)
	}
	weaker, conflict := *previous, *previous
	weaker.SubmissionId, conflict.SubmissionId = "synthetic-weaker-alias", "synthetic-conflicting-alias"
	conflict.EvaluationFailure = &EvaluationFailure{Kind: "submission", Code: "run_unstable"}
	before := []ApexAdapterRecord{*current, weaker, conflict}
	handled, err := reconcileApexReevaluationAssociations([]*ApexAdapterRecord{current, &weaker, &conflict}, now)
	if !handled || err == nil || !reflect.DeepEqual(before, []ApexAdapterRecord{*current, weaker, conflict}) {
		t.Fatalf("late alias conflict partially advanced cached observations: handled=%t error=%v", handled, err)
	}
	if handled, err := reconcileApexReevaluationAssociations(nil, now); handled || err != nil {
		t.Fatalf("empty association group was not a no-op: %t %v", handled, err)
	}
	if _, err := reconcileApexReevaluationAssociations([]*ApexAdapterRecord{nil}, now); err == nil {
		t.Fatal("missing association was accepted")
	}
}

// A new cache alias cannot bypass an existing job reservation, and a failure
// without a cycle marker remains a failure rather than an implicit retry grant.
func TestCompetitionReevaluationApexRefreshSharesBackoffAcrossAliases(t *testing.T) {
	store, name, poll, now := newApexReevaluationFixture(t, false)
	failed := poll
	failed.Reevaluation, failed.State, failed.EvaluationStatus = nil, "completed", "failed"
	failed.EvaluationFailure = &EvaluationFailure{Kind: "infrastructure", Code: "artifact_archive_failed"}
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Header.Get("Authorization") != "Bearer synthetic-alias-owner" {
			t.Error("alias refresh omitted owner authentication")
		}
		writeCompetitionJson(w, http.StatusOK, failed)
	}))
	defer api.Close()
	adapter, err := NewApexAdapter(api.URL, "synthetic-alias-owner", store, &apexConformanceFeeCollector{},
		ApexAdapterOptions{HttpClient: api.Client(), MaxAttempts: 1, Now: func() time.Time { return now }, PrivateScoreRefreshEvery: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if record, err := adapter.PollNext(context.Background()); err != nil || record == nil || record.SubmissionId != name || record.Reevaluation != nil || calls.Load() != 1 {
		t.Fatalf("first failed probe changed authority: %#v %v calls=%d", record, err, calls.Load())
	}
	alias := "synthetic-backoff-cache-alias"
	preparePrivateScoreAssociation(t, store, alias, RoundResult{RoundId: poll.RoundId}, now)
	if _, err := store.RecordAdmission(alias, ScoreAcceptedResult{JobId: poll.JobId, RoundId: poll.RoundId, PatchSha256: poll.PatchSha256,
		State: "completed", CacheHit: true, StatusUrl: "/competition/score/" + poll.JobId.String()}, now); err != nil {
		t.Fatal(err)
	}
	if record, err := adapter.PollNext(context.Background()); err != nil || record != nil || calls.Load() != 1 {
		t.Fatalf("alias bypassed job-keyed refresh backoff: %#v %v calls=%d", record, err, calls.Load())
	}
}

// Read-only discovery excludes non-remedial failure classes and any cycle
// whose one authorization has already been consumed.
func TestCompetitionReevaluationApexRefreshExcludesGenuineAndUsedFailures(t *testing.T) {
	store, name, poll, now := newApexReevaluationFixture(t, false)
	record, err := store.Get(name)
	if err != nil || !apexReevaluationRefreshEligible(*record) {
		t.Fatalf("synthetic infrastructure failure is not probe eligible: %#v %v", record, err)
	}
	for _, testCase := range []struct {
		name   string
		mutate func(*ApexAdapterRecord)
	}{
		{name: "genuine resource failure", mutate: func(record *ApexAdapterRecord) {
			record.EvaluationFailure = &EvaluationFailure{Kind: "submission", Code: "resource_violation"}
		}},
		{name: "canceled", mutate: func(record *ApexAdapterRecord) { record.EvaluationStatus = "canceled" }},
		{name: "staging", mutate: func(record *ApexAdapterRecord) { record.Staging = true }},
		{name: "published", mutate: func(record *ApexAdapterRecord) { record.Published = true }},
		{name: "unknown infrastructure code", mutate: func(record *ApexAdapterRecord) {
			record.EvaluationFailure = &EvaluationFailure{Kind: "infrastructure", Code: "synthetic_unknown"}
		}},
		{name: "already used cycle", mutate: func(record *ApexAdapterRecord) { record.Reevaluation = cloneApexReevaluationCycle(poll.Reevaluation) }},
	} {
		copy := *record
		testCase.mutate(&copy)
		if apexReevaluationRefreshEligible(copy) {
			t.Errorf("%s: non-remedial observation became refresh eligible", testCase.name)
		}
	}
	if _, err := store.RecordPoll(name, poll, now); err != nil {
		t.Fatal(err)
	}
}
