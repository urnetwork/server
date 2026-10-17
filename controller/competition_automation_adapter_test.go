// Deterministic protocol fixtures cover replays, crashes, ordering, and absence of payment authority.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Synthetic scope and a fixed clock make every transition independent of scheduler timing.
func rewardAdapterFixture(t *testing.T) (ApexRewardScope, time.Time, model.CompetitionRewardEvent, model.CompetitionRewardEvent) {
	t.Helper()
	scope := ApexRewardScope{Environment: "test-env", CompetitionId: "test-competition", SeasonId: "test-season"}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	roundId := server.RequireParseId("00000000-0000-0000-0000-000000000101")
	jobId := server.RequireParseId("00000000-0000-0000-0000-000000000102")
	anchor := now.Add(-time.Hour)
	seal := rewardAdapterSeal(t, scope, roundId, jobId, now, anchor)
	first := model.CompetitionRewardDecision{RoundId: roundId, Environment: scope.Environment, CompetitionId: scope.CompetitionId, SeasonId: scope.SeasonId,
		Revision: 1, SealSha256: seal.SealSha256, Action: "elect", SelectedJobId: &jobId, ReasonCode: "first_eligible", DecidedAt: now,
		EffectiveSettlementAnchor: &anchor, SettlementPolicyVersion: "test-settlement/1", BurnPolicyVersion: "test-burn/1", SettlementMode: "shadow"}
	first.DecisionSha256, _ = model.CompetitionAutomationDigest(first)
	firstPayload, _ := model.CompetitionAutomationDigest(first)
	firstEvent := model.CompetitionRewardEvent{Cursor: 3, EventId: server.RequireParseId("00000000-0000-0000-0000-000000000201"), PayloadSha256: firstPayload, PublicationAt: now, Decision: first}
	second := model.CompetitionRewardDecision{RoundId: roundId, Environment: scope.Environment, CompetitionId: scope.CompetitionId, SeasonId: scope.SeasonId,
		Revision: 2, PreviousDecisionSha256: first.DecisionSha256, SealSha256: first.SealSha256, Action: "no_winner", SupersededJobId: &jobId,
		SupersededRevision: 1, ReasonCode: "all_eligible_disqualified", DecidedAt: now, SettlementPolicyVersion: first.SettlementPolicyVersion,
		BurnPolicyVersion: first.BurnPolicyVersion, SettlementMode: "shadow"}
	reviewId := server.RequireParseId("00000000-0000-0000-0000-000000000301")
	second.ReviewId = &reviewId
	second.DecisionSha256, _ = model.CompetitionAutomationDigest(second)
	secondPayload, _ := model.CompetitionAutomationDigest(second)
	secondEvent := model.CompetitionRewardEvent{Cursor: 8, EventId: server.RequireParseId("00000000-0000-0000-0000-000000000202"), PayloadSha256: secondPayload, PublicationAt: now, Decision: second}
	return scope, now, firstEvent, secondEvent
}

// The fixture uses the same selector and digest identities as real persisted seals.
func rewardAdapterSeal(t *testing.T, scope ApexRewardScope, roundId, jobId server.Id, now, anchor time.Time) model.CompetitionEvaluationSeal {
	t.Helper()
	raw, normalized := 80.0, 125.0
	significance := testScoreSignificance(true)
	significance.ReplicateCount = 9
	// Median controls the takeover boundary; the independent Welch mean differs.
	significance.BaselineMeanRawScore = 90
	significance.CandidateMeanRawScore = 81
	score := &ScoreResult{ScoreSchema: ScoreSchema, RawScore: &raw, NormalizedScore: &normalized, Placeable: true, TakeoverEligible: true, Significance: significance, Gates: map[string]Gate{}}
	for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources"} {
		score.Gates[name] = Gate{Passed: true, Details: map[string]any{}}
	}
	seal := model.CompetitionEvaluationSeal{RoundId: roundId, Environment: scope.Environment, CompetitionId: scope.CompetitionId, SeasonId: scope.SeasonId, Epoch: 1,
		SealedAt: now, PublicationAt: now, SourceChoiceAt: now, PolicySha256: strings.Repeat("b", 64), BaselineSha256: strings.Repeat("c", 64), BaselineRawScore: 100, TakeoverMargin: .12, TerminalCounts: map[string]int{"succeeded": 1},
		Entries: []model.CompetitionSealedEntry{{JobId: jobId, PatchSha256: strings.Repeat("d", 64), BaseSourceLockSha256: strings.Repeat("e", 64), State: "succeeded", SubmittedAt: anchor.Add(-time.Hour), CompletedAt: anchor, Score: score, ScoreSha256: strings.Repeat("f", 64), ArtifactManifestSha256: strings.Repeat("a", 64)}},
	}
	seal.InventorySha256, _ = model.CompetitionAutomationDigest(seal.Entries)
	var err error
	seal.Entries, err = model.RankCompetitionSealedEntries(seal.Entries, 100, .12, nil)
	if err != nil {
		t.Fatal(err)
	}
	seal.RankingSha256, _ = model.CompetitionAutomationDigest(seal.Entries)
	seal.SealSha256, _ = model.CompetitionAutomationDigest(seal)
	return seal
}

// Supplies complete reconciliation history, including the genuine replacement finding.
func rewardAdapterResult(t *testing.T, scope ApexRewardScope, first, second model.CompetitionRewardEvent) model.CompetitionV2RoundResult {
	t.Helper()
	seal := rewardAdapterSeal(t, scope, first.Decision.RoundId, *first.Decision.SelectedJobId, first.PublicationAt, *first.Decision.EffectiveSettlementAnchor)
	entry := seal.Entries[0]
	request := model.CompetitionIntegrityReviewRequest{ExpectedRevision: 1, JobId: entry.JobId, PatchSha256: entry.PatchSha256, BaseSourceLockSha256: entry.BaseSourceLockSha256,
		ScoreSha256: entry.ScoreSha256, BaselineSha256: seal.BaselineSha256, ArtifactManifestSha256: entry.ArtifactManifestSha256, CriteriaVersion: "test-review/1",
		RewardDisposition: "disqualified", SourceDisposition: "rejected", ReasonCodes: []string{"SCORE_TAMPERING"}, PublicExplanation: "Synthetic measurement integrity finding.", WithheldEvidenceExplanation: "Synthetic private reproduction is withheld."}
	for _, criterion := range []string{"measurement_integrity", "workload_generalization", "containment", "patch_scope", "reproducibility", "source_safety"} {
		request.Findings = append(request.Findings, model.CompetitionReviewFinding{Criterion: criterion, Disposition: "pass", ReasonCode: "APPROVED"})
	}
	request.Findings[0].Disposition, request.Findings[0].ReasonCode = "fail", "SCORE_TAMPERING"
	return model.CompetitionV2RoundResult{DecisionMode: model.CompetitionDecisionModeV2, RequiredApiCapability: model.CompetitionAutomationCapability, Seal: seal, RewardRevision: 2,
		Decisions: []model.CompetitionRewardDecision{first.Decision, second.Decision}, IntegrityReview: "not_applicable", SourceApproval: "not_applicable", SettlementStatus: "pending",
		IntegrityReviews: []model.CompetitionIntegrityReviewResult{{ReviewId: *second.Decision.ReviewId, RoundId: seal.RoundId, CreatedAt: second.Decision.DecidedAt,
			Request: request, RewardDecision: second.Decision}},
	}
}

// Gaps in global cursors are allowed; gaps in one round's revision chain are not.
func TestApexRewardInboxReplayAndRestart(t *testing.T) {
	scope, now, first, second := rewardAdapterFixture(t)
	directory := filepath.Join(t.TempDir(), "private-inbox")
	store, err := NewApexRewardFileStore(directory, scope)
	if err != nil {
		t.Fatal(err)
	}
	page := model.CompetitionRewardEventPage{Events: []model.CompetitionRewardEvent{first, second}, NextCursor: second.Cursor}
	if err := store.RecordPage(page, now); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPage(page, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewApexRewardFileStore(directory, scope)
	if err != nil {
		t.Fatal(err)
	}
	cursor, records, err := reopened.Snapshot()
	if err != nil || cursor != second.Cursor || len(records) != 2 || !records[0].ReceivedAt.Equal(now) || records[1].Event.Decision.SelectedJobId != nil {
		t.Fatalf("durable revision replay = cursor %d, records %+v, error %v", cursor, records, err)
	}
	otherScope := scope
	otherScope.SeasonId = "other-season"
	if _, err := NewApexRewardFileStore(directory, otherScope); err == nil {
		t.Fatal("inbox changed its persisted scope")
	}
}

// A failed transaction leaves the previous cursor and original event untouched.
func TestApexRewardInboxRejectsConflictsAndRevisionGaps(t *testing.T) {
	scope, now, first, second := rewardAdapterFixture(t)
	store, err := NewApexRewardFileStore(filepath.Join(t.TempDir(), "private-inbox"), scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPage(model.CompetitionRewardEventPage{Events: []model.CompetitionRewardEvent{second}, NextCursor: second.Cursor}, now); err == nil {
		t.Fatal("revision two accepted before revision one")
	}
	if err := store.RecordPage(model.CompetitionRewardEventPage{Events: []model.CompetitionRewardEvent{first}, NextCursor: first.Cursor}, now); err != nil {
		t.Fatal(err)
	}
	conflicts := []model.CompetitionRewardEvent{first, first, first, second}
	conflicts[0].PublicationAt = now.Add(-time.Second)
	conflicts[1].EventId = second.EventId
	conflicts[2].Cursor = second.Cursor
	conflicts[3].Decision.PreviousDecisionSha256 = strings.Repeat("f", 64)
	for _, conflict := range conflicts {
		if err := store.RecordPage(model.CompetitionRewardEventPage{Events: []model.CompetitionRewardEvent{conflict}, NextCursor: conflict.Cursor}, now); err == nil {
			t.Fatalf("accepted conflicting event %+v", conflict)
		}
		cursor, records, err := store.Snapshot()
		if err != nil || cursor != first.Cursor || len(records) != 1 {
			t.Fatalf("conflict mutated durable state: %d %d %v", cursor, len(records), err)
		}
	}
	if err := store.RecordPage(model.CompetitionRewardEventPage{Events: nil, NextCursor: second.Cursor}, now); err == nil {
		t.Fatal("empty page advanced past unpersisted decisions")
	}
}

// Neither a valid digest nor a settlement token bypasses publication or scope.
func TestApexRewardInboxRejectsEmbargoAndTampering(t *testing.T) {
	scope, now, first, _ := rewardAdapterFixture(t)
	mutations := []model.CompetitionRewardEvent{first, first, first, first}
	mutations[0].PublicationAt = now.Add(time.Second)
	mutations[1].PayloadSha256 = strings.Repeat("0", 64)
	mutations[2].Decision.CompetitionId = "other-competition"
	mutations[3].Decision.SelectedJobId = nil
	for _, event := range mutations {
		if validateApexRewardEvent(event, scope, now) == nil {
			t.Fatalf("accepted embargoed or tampered event %+v", event)
		}
	}
}

// The reference adapter has no mode that treats receipt as applied or confirmed money.
func TestApexRewardReceiptCannotClaimPayment(t *testing.T) {
	scope, now, event, _ := rewardAdapterFixture(t)
	store, err := NewApexRewardFileStore(filepath.Join(t.TempDir(), "private-inbox"), scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPage(model.CompetitionRewardEventPage{Events: []model.CompetitionRewardEvent{event}, NextCursor: event.Cursor}, now); err != nil {
		t.Fatal(err)
	}
	receipt := model.CompetitionSettlementReceipt{EventId: event.EventId, Environment: scope.Environment, CompetitionId: scope.CompetitionId, SeasonId: scope.SeasonId,
		RoundId: event.Decision.RoundId, Revision: 1, PayloadSha256: event.PayloadSha256, DecisionSha256: event.Decision.DecisionSha256,
		Status: "applied", AccountingReference: "test-accounting-reference", AppliedAt: &now, ReceivedAt: now, ActorId: "test-settlement"}
	if err := store.RecordReceipt(receipt); err == nil {
		t.Fatal("reference inbox pretended to apply a payment")
	}
	receipt.Status, receipt.AppliedAt = "received", nil
	if err := store.RecordReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	receipt.AccountingReference = "conflicting-reference"
	if err := store.RecordReceipt(receipt); err == nil {
		t.Fatal("conflicting receipt changed accounting history")
	}
}

// A server-committed acknowledgment whose response is lost is replayed after process restart.
func TestApexRewardAdapterRecoversLostAcknowledgmentAndMissedRevision(t *testing.T) {
	scope, now, first, second := rewardAdapterFixture(t)
	result := rewardAdapterResult(t, scope, first, second)
	acknowledged := map[server.Id]model.CompetitionSettlementReceipt{}
	postCalls := 0
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/result") && r.Header.Get("Authorization") != "Bearer synthetic-settlement-token" {
			t.Errorf("missing settlement authentication")
			w.WriteHeader(401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /competition/v2/events":
			page := model.CompetitionRewardEventPage{Events: []model.CompetitionRewardEvent{first}, NextCursor: first.Cursor}
			if r.URL.Query().Get("after_cursor") == "3" {
				page = model.CompetitionRewardEventPage{Events: []model.CompetitionRewardEvent{second}, NextCursor: second.Cursor}
			}
			if r.URL.Query().Get("after_cursor") == "8" {
				page = model.CompetitionRewardEventPage{Events: []model.CompetitionRewardEvent{}, NextCursor: second.Cursor}
			}
			json.NewEncoder(w).Encode(page)
		case "POST /competition/v2/receipts":
			var receipt model.CompetitionSettlementReceipt
			if err := json.NewDecoder(r.Body).Decode(&receipt); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if receipt.Status != "received" || receipt.ChainTransaction != "" || receipt.AppliedAt != nil {
				t.Error("adapter asserted an unsupported money effect")
			}
			postCalls++
			if previous, ok := acknowledged[receipt.EventId]; ok {
				receipt = previous
			} else {
				receipt.ActorId = "test-settlement"
				receipt.ReceivedAt = now
				acknowledged[receipt.EventId] = receipt
			}
			if postCalls == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(receipt)
		default:
			if r.Method == http.MethodGet && r.URL.Path == "/competition/v2/rounds/"+first.Decision.RoundId.String()+"/result" {
				json.NewEncoder(w).Encode(result)
				return
			}
			t.Errorf("unexpected integration call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer apiServer.Close()
	directory := filepath.Join(t.TempDir(), "private-inbox")
	store, err := NewApexRewardFileStore(directory, scope)
	if err != nil {
		t.Fatal(err)
	}
	options := ApexRewardAdapterOptions{Enabled: true, HttpClient: apiServer.Client(), MaxAttempts: 1, Now: func() time.Time { return now }}
	adapter, err := NewApexRewardAdapter(apiServer.URL, "synthetic-settlement-token", store, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Reconcile(context.Background()); err == nil {
		t.Fatal("lost acknowledgment did not surface")
	}
	cursor, records, err := store.Snapshot()
	if err != nil || cursor != first.Cursor || len(records) != 1 || records[0].Receipt != nil {
		t.Fatalf("inbox not durable before send: %d %+v %v", cursor, records, err)
	}
	reopened, err := NewApexRewardFileStore(directory, scope)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err = NewApexRewardAdapter(apiServer.URL, "synthetic-settlement-token", reopened, options)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := adapter.Reconcile(context.Background()); err != nil || count != 1 {
		t.Fatalf("restart reconciliation = %d %v", count, err)
	}
	if count, err := adapter.Reconcile(context.Background()); err != nil || count != 0 {
		t.Fatalf("caught-up reconciliation = %d %v", count, err)
	}
	cursor, records, err = reopened.Snapshot()
	if err != nil || cursor != second.Cursor || len(records) != 2 || records[0].Receipt == nil || records[1].Receipt == nil || postCalls != 3 || len(acknowledged) != 2 {
		t.Fatalf("reconciliation = %d %+v, posts %d, receipts %d, error %v", cursor, records, postCalls, len(acknowledged), err)
	}
}

// Configuration presence never silently enables v2 reconciliation.
func TestApexRewardAdapterRequiresExplicitEnablement(t *testing.T) {
	scope, _, _, _ := rewardAdapterFixture(t)
	store, err := NewApexRewardFileStore(filepath.Join(t.TempDir(), "private-inbox"), scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewApexRewardAdapter("https://api.example", "synthetic-settlement-token", store, ApexRewardAdapterOptions{}); err == nil {
		t.Fatal("disabled adapter constructed")
	}
	if _, err := NewApexRewardAdapter("http://api.example", "synthetic-settlement-token", store, ApexRewardAdapterOptions{Enabled: true}); err == nil {
		t.Fatal("non-loopback plaintext adapter constructed")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	adapter, err := NewApexRewardAdapter("https://api.example", "synthetic-settlement-token", store, ApexRewardAdapterOptions{Enabled: true, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Reconcile(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

// A legacy consumer refuses an advertised v2 round before collecting even one fee.
func TestApexLegacyAdapterRefusesV2BeforeFee(t *testing.T) {
	scope, now, event, _ := rewardAdapterFixture(t)
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/competition/info" {
			t.Errorf("legacy adapter attempted %s", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(InfoResult{CompetitionId: scope.CompetitionId, ActiveRound: &RoundResult{RoundId: event.Decision.RoundId, Status: "open", DecisionMode: model.CompetitionDecisionModeV2, RequiredApiCapability: model.CompetitionAutomationCapability}})
	}))
	defer apiServer.Close()
	store, err := NewApexAdapterFileStore(filepath.Join(t.TempDir(), "private-admissions"))
	if err != nil {
		t.Fatal(err)
	}
	fees := &apexConformanceFeeCollector{receipts: map[string]string{}}
	adapter, err := NewApexAdapter(apiServer.URL, "synthetic-submitter-token", store, fees, ApexAdapterOptions{HttpClient: apiServer.Client(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Submit(context.Background(), "test-submission", []byte(testPatch("test-change"))); err == nil || fees.calls != 0 {
		t.Fatalf("unsupported mode charged a fee: calls=%d error=%v", fees.calls, err)
	}
}

// Known duplicate entitlements are screened before the normal fee collector side effect.
func TestApexV2AdapterScreensDuplicateBeforeFee(t *testing.T) {
	scope, now, event, _ := rewardAdapterFixture(t)
	preflightCalls := 0
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/competition/info":
			json.NewEncoder(w).Encode(InfoResult{CompetitionId: scope.CompetitionId, ActiveRound: &RoundResult{RoundId: event.Decision.RoundId, Status: "open", DecisionMode: model.CompetitionDecisionModeV2, RequiredApiCapability: model.CompetitionAutomationCapability}})
		case "/competition/v2/score/preflight":
			preflightCalls++
			json.NewEncoder(w).Encode(model.CompetitionV2SubmissionPreflight{RoundId: event.Decision.RoundId, PatchSha256: strings.Repeat("b", 64), BaseSourceLockSha256: strings.Repeat("c", 64), DecisionMode: model.CompetitionDecisionModeV2, RequiredApiCapability: model.CompetitionAutomationCapability, RewardEligible: false, IneligibilityReason: "reward_duplicate"})
		default:
			t.Errorf("unexpected admission call %s", r.URL.Path)
			w.WriteHeader(400)
		}
	}))
	defer apiServer.Close()
	store, err := NewApexAdapterFileStore(filepath.Join(t.TempDir(), "private-admissions"))
	if err != nil {
		t.Fatal(err)
	}
	fees := &apexConformanceFeeCollector{receipts: map[string]string{}}
	adapter, err := NewApexAdapter(apiServer.URL, "synthetic-submitter-token", store, fees, ApexAdapterOptions{HttpClient: apiServer.Client(), V2CompetitionId: scope.CompetitionId, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Submit(context.Background(), "test-submission", []byte(testPatch("test-change"))); err == nil || fees.calls != 0 || preflightCalls != 1 {
		t.Fatalf("preflight did not prevent fee: calls=%d preflights=%d error=%v", fees.calls, preflightCalls, err)
	}
}

// Persisted round identity cannot be reinterpreted as another explicitly enabled competition.
func TestApexV2AdmissionKeepsCompetitionScopeAcrossRestart(t *testing.T) {
	scope, now, event, _ := rewardAdapterFixture(t)
	directory := filepath.Join(t.TempDir(), "private-admissions")
	store, err := NewApexAdapterFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginSubmission("test-submission", strings.Repeat("a", 64), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.recordRound("test-submission", RoundResult{RoundId: event.Decision.RoundId, DecisionMode: model.CompetitionDecisionModeV2, RequiredApiCapability: model.CompetitionAutomationCapability}, scope.CompetitionId, now); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewApexAdapterFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	record, err := reopened.Get("test-submission")
	if err != nil {
		t.Fatal(err)
	}
	if record.CompetitionId != scope.CompetitionId {
		t.Fatal("v2 competition scope was not persisted")
	}
	if validateApexAdmissionContract(record.CompetitionId, RoundResult{DecisionMode: record.DecisionMode, RequiredApiCapability: record.RequiredApiCapability}, "other-competition") == nil {
		t.Fatal("restart changed the explicitly enabled competition")
	}
}
