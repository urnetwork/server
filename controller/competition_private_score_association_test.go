// Cache-association regressions preserve many miner admissions for one immutable
// job without broadening ownership or multiplying polling/reward identities.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Independent fee/input identities deliberately differ even when canonical
// patch identity later converges through an authoritative API admission.
func preparePrivateScoreAssociation(t testing.TB, store *ApexAdapterFileStore, name string, round RoundResult, now time.Time) {
	t.Helper()
	digest := sha256.Sum256([]byte(name))
	if _, err := store.BeginSubmission(name, hex.EncodeToString(digest[:]), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.recordRound(name, round, "synthetic-competition", now); err != nil {
		t.Fatal(err)
	}
	if !round.Staging {
		if _, err := store.RecordFee(name, "synthetic-fee-"+name, now); err != nil {
			t.Fatal(err)
		}
	}
}

// Two real adapter submissions of identical bytes share the API's cached job,
// not their miner/fee identities. A single poll and public entry update both.
func TestCompetitionPrivateScoreCanonicalCacheKeepsMultipleMinerAdmissions(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	roundId, jobId := server.NewId(), server.NewId()
	patch := []byte("synthetic-identical-canonical-patch\n")
	digest := sha256.Sum256(patch)
	patchSha256 := hex.EncodeToString(digest[:])
	score := privateScoreFeedbackFixture()
	delete(score.Gates, "synthetic-private-gate-key")
	score.Placeable, score.TakeoverEligible = true, true
	score.Significance = testScoreSignificance(true)
	score.Gates["G2_volume"] = Gate{Passed: true, Details: map[string]any{
		"candidate_requests": 1000.0, "request_min": 950.0, "request_max": 1050.0,
		"candidate_bytes": 10000.0, "bytes_min": 9500.0, "bytes_max": 10500.0,
	}}
	var stateLock sync.Mutex
	admissionCalls, pollCalls := 0, 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/competition/info":
			writeCompetitionJson(w, http.StatusOK, InfoResult{CompetitionId: "synthetic-competition", ActiveRound: &RoundResult{RoundId: roundId, Status: "open"}})
		case r.Method == http.MethodPost && r.URL.Path == "/competition/score":
			if r.Header.Get("Authorization") != "Bearer synthetic-shared-miner-credential" {
				t.Error("cache admission lost the credential boundary")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var args ScoreArgs
			if err := json.NewDecoder(r.Body).Decode(&args); err != nil || args.RoundId != roundId || args.Patch != string(patch) {
				t.Errorf("canonical cache request changed: %#v %v", args, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			stateLock.Lock()
			admissionCalls++
			cached := admissionCalls > 1
			stateLock.Unlock()
			state := "queued"
			if cached {
				state = "completed"
			}
			writeCompetitionJson(w, http.StatusAccepted, ScoreAcceptedResult{JobId: jobId, RoundId: roundId, PatchSha256: patchSha256,
				State: state, CacheHit: cached, StatusUrl: "/competition/score/" + jobId.String()})
		case r.Method == http.MethodGet && r.URL.Path == "/competition/score/"+jobId.String():
			if r.Header.Get("Authorization") != "Bearer synthetic-shared-miner-credential" {
				t.Error("private cache polling lost the credential boundary")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			stateLock.Lock()
			pollCalls++
			stateLock.Unlock()
			writeCompetitionJson(w, http.StatusOK, ScoreJobResult{JobId: jobId, RoundId: roundId, PatchSha256: patchSha256,
				State: "completed", EvaluationStatus: "completed", Score: privateCompletedScore(score)})
		case r.Method == http.MethodGet && r.URL.Path == "/competition/leaderboard":
			if r.Header.Get("Authorization") != "" {
				t.Error("public reconciliation carried a shared miner credential")
			}
			writeCompetitionJson(w, http.StatusOK, SeasonLeaderboardResult{CompetitionId: "synthetic-competition", Epochs: []LeaderboardResult{{
				RoundId: roundId, Status: "finalized", FinalizedAt: now, WinnerJobId: &jobId,
				Entries: []LeaderboardEntry{{Rank: 1, JobId: jobId, PatchSha256: patchSha256, Score: *score, Winner: true, HonestyReview: "approved"}},
			}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()
	directory := filepath.Join(t.TempDir(), "private-state")
	store, err := NewApexAdapterFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	fees := &apexConformanceFeeCollector{receipts: map[string]string{}}
	options := ApexAdapterOptions{HttpClient: api.Client(), MaxAttempts: 1, Now: func() time.Time { return now }}
	adapter, err := NewApexAdapter(api.URL, "synthetic-shared-miner-credential", store, fees, options)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"synthetic-miner-a-admission", "synthetic-miner-b-admission"}
	for _, name := range names {
		if _, err := adapter.Submit(context.Background(), name, patch); err != nil {
			t.Fatalf("legitimate duplicate canonical admission %s: %v", name, err)
		}
	}
	if record, err := adapter.PollNext(context.Background()); err != nil || record == nil || record.SubmissionId != names[0] {
		t.Fatalf("shared job private poll = %#v %v", record, err)
	}
	for i, name := range names {
		record, err := store.Get(name)
		if err != nil || record.SubmissionId != name || record.Sequence != uint64(i+1) || record.JobId != jobId || record.PrivateScore == nil ||
			record.Score != nil || record.Published || record.Winner || record.HonestyReview != "" || record.FeeReceipt != "receipt-"+name {
			t.Fatalf("private cached miner association %s = %#v %v", name, record, err)
		}
	}
	if _, err := store.Get("synthetic-unadmitted-miner"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private feedback invented an unadmitted association: %v", err)
	}
	reopened, err := NewApexAdapterFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err = NewApexAdapter(api.URL, "synthetic-shared-miner-credential", reopened, fees, options)
	if err != nil {
		t.Fatal(err)
	}
	if record, err := adapter.PollNext(context.Background()); err != nil || record != nil {
		t.Fatalf("shared completed job polled once per miner after restart: %#v %v", record, err)
	}
	boards, err := adapter.Reconcile(context.Background())
	if err != nil || len(boards.Epochs) != 1 || len(boards.Epochs[0].Entries) != 1 {
		t.Fatalf("shared job public reconciliation = %#v %v", boards, err)
	}
	for _, name := range names {
		record, err := reopened.Get(name)
		if err != nil || record.JobId != jobId || !record.Published || record.Score == nil || record.PrivateScore != nil ||
			!record.Winner || record.HonestyReview != "approved" || record.FeeReceipt != "receipt-"+name {
			t.Fatalf("public cached miner association %s = %#v %v", name, record, err)
		}
	}
	stateLock.Lock()
	observedAdmissions, observedPolls := admissionCalls, pollCalls
	stateLock.Unlock()
	if observedAdmissions != 2 || observedPolls != 1 || fees.calls != 2 {
		t.Fatalf("cache identities multiplied: admissions=%d polls=%d fees=%d", observedAdmissions, observedPolls, fees.calls)
	}
}

// A delayed accepted cache receipt can inherit an already observed private or
// public outcome, but never another miner's admission-local identity or fees.
func TestCompetitionPrivateScoreCachedAssociationsBackfillKnownOutcomes(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		store, err := NewApexAdapterFileStore(filepath.Join(t.TempDir(), "private-state"))
		if err != nil {
			t.Fatal(err)
		}
		now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		round := RoundResult{RoundId: server.NewId()}
		jobId := server.NewId()
		accepted := ScoreAcceptedResult{JobId: jobId, RoundId: round.RoundId, State: "completed", CacheHit: true,
			PatchSha256: strings.Repeat("b", 64), StatusUrl: "/competition/score/" + jobId.String()}
		if v2 {
			round.DecisionMode, round.RequiredApiCapability = model.CompetitionDecisionModeV2, model.CompetitionAutomationCapability
			accepted.DecisionMode, accepted.RequiredApiCapability = round.DecisionMode, round.RequiredApiCapability
			accepted.StatusUrl = "/competition/v2/score/" + jobId.String()
		}
		names := []string{"synthetic-cache-owner-a", "synthetic-cache-owner-b", "synthetic-delayed-cache-receipt"}
		for _, name := range names {
			preparePrivateScoreAssociation(t, store, name, round, now)
		}
		if _, err := store.RecordAdmission(names[0], accepted, now); err != nil {
			t.Fatal(err)
		}
		poll := ScoreJobResult{JobId: jobId, RoundId: round.RoundId, PatchSha256: accepted.PatchSha256, State: "completed", EvaluationStatus: "completed",
			Score: privateCompletedScore(privateScoreFeedbackFixture()), DecisionMode: accepted.DecisionMode, RequiredApiCapability: accepted.RequiredApiCapability}
		if _, err := store.RecordPoll(names[0], poll, now); err != nil {
			t.Fatal(err)
		}
		record, err := store.RecordAdmission(names[1], accepted, now)
		if err != nil || record.PrivateScore == nil || record.Published || record.SubmissionId != names[1] || record.FeeReceipt != "synthetic-fee-"+names[1] {
			t.Fatalf("v2=%t existing private observation did not backfill its legitimate alias: %#v %v", v2, record, err)
		}
		if v2 {
			published := poll
			published.State, published.Score = "succeeded", privateScoreFeedbackFixture()
			if _, err := store.RecordPoll(names[0], published, now); err != nil {
				t.Fatal(err)
			}
		} else if err := store.ReconcileLeaderboard(SeasonLeaderboardResult{Epochs: []LeaderboardResult{{
			RoundId: round.RoundId, Status: "finalized", FinalizedAt: now,
			Entries: []LeaderboardEntry{{Rank: 1, JobId: jobId, PatchSha256: accepted.PatchSha256, Score: *privateScoreFeedbackFixture(), HonestyReview: "not_reviewed"}},
		}}}, now); err != nil {
			t.Fatal(err)
		}
		record, err = store.RecordAdmission(names[2], accepted, now)
		if err != nil || !record.Published || record.Score == nil || record.PrivateScore != nil || record.SubmissionId != names[2] || record.FeeReceipt != "synthetic-fee-"+names[2] {
			t.Fatalf("v2=%t delayed accepted receipt did not inherit its public job observation: %#v %v", v2, record, err)
		}
		for _, name := range names {
			record, err := store.Get(name)
			if err != nil || !record.Published || record.Score == nil || record.SubmissionId != name || record.FeeReceipt != "synthetic-fee-"+name {
				t.Fatalf("v2=%t backfill mixed miner identities: %#v %v", v2, record, err)
			}
		}
	}
}

// Two individually valid v2 admission rows may share a job only when every
// immutable server identity and contract field agrees exactly.
func privateScoreAssociationState() apexAdapterState {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	jobId := server.NewId()
	first := ApexAdapterRecord{Sequence: 1, SubmissionId: "synthetic-associated-a", InputPatchSha256: strings.Repeat("a", 64),
		JobId: jobId, RoundId: server.NewId(), CanonicalPatchSha256: strings.Repeat("c", 64), StatusUrl: "/competition/v2/score/" + jobId.String(),
		FeeUsd: apexSubmissionFeeUsd, FeeReceipt: "synthetic-fee-a", State: "queued", EvaluationStatus: "queued", SubmittedAt: now, UpdatedAt: now,
		DecisionMode: model.CompetitionDecisionModeV2, RequiredApiCapability: model.CompetitionAutomationCapability, CompetitionId: "synthetic-competition"}
	second := first
	second.Sequence, second.SubmissionId = 2, "synthetic-associated-b"
	second.InputPatchSha256, second.FeeReceipt = strings.Repeat("b", 64), "synthetic-fee-b"
	return apexAdapterState{Schema: apexAdapterStateSchema, NextSequence: 2, Records: []ApexAdapterRecord{first, second}}
}

// Job sharing is exact identity equality, not a blanket removal of uniqueness.
func TestCompetitionPrivateScoreCachedAssociationsRejectIdentityMixing(t *testing.T) {
	valid := privateScoreAssociationState()
	if err := validateApexAdapterState(&valid); err != nil {
		t.Fatalf("legitimate canonical job associations rejected: %v", err)
	}
	for _, c := range []struct {
		name   string
		mutate func(*ApexAdapterRecord)
	}{
		{name: "round", mutate: func(record *ApexAdapterRecord) { record.RoundId = server.NewId() }},
		{name: "canonical patch", mutate: func(record *ApexAdapterRecord) { record.CanonicalPatchSha256 = strings.Repeat("d", 64) }},
		{name: "status URL", mutate: func(record *ApexAdapterRecord) { record.StatusUrl += "?synthetic-mismatch=1" }},
		{name: "staging", mutate: func(record *ApexAdapterRecord) { record.Staging, record.FeeUsd, record.FeeReceipt = true, 0, "" }},
		{name: "decision mode", mutate: func(record *ApexAdapterRecord) {
			record.DecisionMode, record.RequiredApiCapability, record.CompetitionId = "", "", ""
		}},
		{name: "runtime capability", mutate: func(record *ApexAdapterRecord) { record.RequiredApiCapability = model.CompetitionRuntimeBlobCapability }},
		{name: "competition scope", mutate: func(record *ApexAdapterRecord) { record.CompetitionId = "synthetic-other-competition" }},
	} {
		state := privateScoreAssociationState()
		c.mutate(&state.Records[1])
		if err := validateApexAdapterState(&state); err == nil {
			t.Errorf("shared cached job accepted inconsistent %s", c.name)
		}
	}
}

// A weak middle association cannot hide conflicting non-adjacent terminal
// observations for the same immutable job.
func TestCompetitionPrivateScoreCachedAssociationsRejectContradictoryObservations(t *testing.T) {
	state := privateScoreAssociationState()
	first := &state.Records[0]
	first.State, first.EvaluationStatus = "completed", "completed"
	first.PrivateScore = privateCompletedScore(privateScoreFeedbackFixture())
	third := *first
	third.Sequence, third.SubmissionId, third.FeeReceipt = 3, "synthetic-associated-c", "synthetic-fee-c"
	third.PrivateScore = privateCompletedScore(privateScoreFeedbackFixture())
	*third.PrivateScore.RawScore = 89
	state.NextSequence = 3
	state.Records = append(state.Records, third)
	if err := validateApexAdapterState(&state); err == nil {
		t.Fatal("weak middle association hid a contradictory private measurement")
	}
	state.Records[2].PrivateScore = nil
	state.Records[2].EvaluationStatus = "failed"
	if err := validateApexAdapterState(&state); err == nil {
		t.Fatal("matching job identity mixed successful and failed terminal outcomes")
	}
}

// A rejected association or unadmitted poll never persists a partial join or
// updates the real miners' records. A corrected authoritative receipt can join.
func TestCompetitionPrivateScoreCachedAssociationRejectionIsAtomic(t *testing.T) {
	store, err := NewApexAdapterFileStore(filepath.Join(t.TempDir(), "private-state"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	round := RoundResult{RoundId: server.NewId()}
	jobId := server.NewId()
	names := []string{"synthetic-atomic-owner-a", "synthetic-atomic-owner-b"}
	for _, name := range names {
		preparePrivateScoreAssociation(t, store, name, round, now)
	}
	accepted := ScoreAcceptedResult{JobId: jobId, RoundId: round.RoundId, PatchSha256: strings.Repeat("c", 64), State: "completed", CacheHit: true,
		StatusUrl: "/competition/score/" + jobId.String()}
	if _, err := store.RecordAdmission(names[0], accepted, now); err != nil {
		t.Fatal(err)
	}
	poll := ScoreJobResult{JobId: jobId, RoundId: round.RoundId, PatchSha256: accepted.PatchSha256, State: "completed", EvaluationStatus: "completed", Score: privateCompletedScore(privateScoreFeedbackFixture())}
	if _, err := store.RecordPoll(names[0], poll, now); err != nil {
		t.Fatal(err)
	}
	wrong := accepted
	wrong.PatchSha256 = strings.Repeat("d", 64)
	if _, err := store.RecordAdmission(names[1], wrong, now); err == nil {
		t.Fatal("cached association accepted a different canonical patch")
	}
	unadmitted, err := store.Get(names[1])
	if err != nil || unadmitted.JobId != (server.Id{}) || unadmitted.PrivateScore != nil || unadmitted.FeeReceipt != "synthetic-fee-"+names[1] {
		t.Fatalf("rejected association persisted a partial identity: %#v %v", unadmitted, err)
	}
	if _, err := store.RecordPoll("synthetic-unadmitted-owner", poll, now); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unadmitted miner acquired job polling association: %v", err)
	}
	if _, err := store.RecordPoll(names[1], poll, now); err == nil {
		t.Fatal("pending but unadmitted miner acquired another admission's feedback")
	}
	joined, err := store.RecordAdmission(names[1], accepted, now)
	if err != nil || joined.PrivateScore == nil || joined.Published || joined.SubmissionId != names[1] {
		t.Fatalf("corrected authoritative cache receipt failed: %#v %v", joined, err)
	}
}

// Each legacy response is normalized independently. Multiple ambiguous cache
// admissions remain unknown until one typed owner poll resolves the job.
func TestCompetitionPrivateScoreCachedLegacyPollDoesNotInventSuccess(t *testing.T) {
	store, err := NewApexAdapterFileStore(filepath.Join(t.TempDir(), "private-state"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	round := RoundResult{RoundId: server.NewId()}
	jobId := server.NewId()
	accepted := ScoreAcceptedResult{JobId: jobId, RoundId: round.RoundId, PatchSha256: strings.Repeat("c", 64), State: "completed", CacheHit: true,
		StatusUrl: "/competition/score/" + jobId.String()}
	names := []string{"synthetic-legacy-associated-a", "synthetic-legacy-associated-b"}
	for _, name := range names {
		preparePrivateScoreAssociation(t, store, name, round, now)
		if _, err := store.RecordAdmission(name, accepted, now); err != nil {
			t.Fatal(err)
		}
	}
	poll := ScoreJobResult{JobId: jobId, RoundId: round.RoundId, PatchSha256: accepted.PatchSha256, State: "completed"}
	if _, err := store.RecordPoll(names[0], poll, now); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		record, err := store.Get(name)
		if err != nil || record.EvaluationStatus != "" || record.PrivateScore != nil || record.Published {
			t.Fatalf("legacy response invented scoring success for %s: %#v %v", name, record, err)
		}
	}
	poll.EvaluationStatus = "failed"
	poll.EvaluationFailure = &EvaluationFailure{Kind: "submission", Code: "candidate_build_failed"}
	if _, err := store.RecordPoll(names[1], poll, now); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		record, err := store.Get(name)
		if err != nil || record.EvaluationStatus != "failed" || record.EvaluationFailure == nil || record.PrivateScore != nil || record.Published || apexPrivateScoreRefreshEligible(*record) {
			t.Fatalf("typed failure did not resolve all existing associations: %#v %v", record, err)
		}
	}
	late := "synthetic-late-failed-cache-association"
	preparePrivateScoreAssociation(t, store, late, round, now)
	record, err := store.RecordAdmission(late, accepted, now)
	if err != nil || record.EvaluationStatus != "failed" || record.EvaluationFailure == nil || record.PrivateScore != nil || record.Published || apexPrivateScoreRefreshEligible(*record) {
		t.Fatalf("known failed cache admission restarted scoring or invented success: %#v %v", record, err)
	}
}
