// Deterministic private-feedback regressions preserve credential ownership,
// useful reviewed measurements, public embargoes and immutable admission state.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
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
	"github.com/urnetwork/server/router"
)

// Every identity and hidden marker is synthetic; a failed G2 still produces
// completed numeric feedback and must not be mistaken for evaluator failure.
func privateScoreFeedbackFixture() *ScoreResult {
	raw, normalized := 90.0, 100.0
	return &ScoreResult{
		ScoreSchema: ScoreSchema, RawScore: &raw, NormalizedScore: &normalized,
		Placeable: false, Significance: testScoreSignificance(false),
		Gates: map[string]Gate{
			"G1_success": {Passed: true, Details: map[string]any{"candidate": .99, "baseline": .99, "absolute_min_inclusive": .95, "baseline_minus_max": .94}},
			"G2_volume": {Passed: false, Details: map[string]any{
				"candidate_requests": 1100.0, "request_min": 950.0, "request_max": 1050.0,
				"candidate_bytes": 11000.0, "bytes_min": 9500.0, "bytes_max": 10500.0,
				"log": "synthetic-private-log", "seed": "synthetic-private-seed",
			}},
			"G3_path_integrity": {Passed: true, Details: map[string]any{"minimum_replicate_coverage": .99, "minimum_inclusive": .95}},
			"G4_matchmaking": {Passed: true, Details: map[string]any{
				"all_replicates_have_samples": true, "all_pools_nonempty": true, "all_replicates_span_at_least_minimum": true,
				"candidate_load_p95_ms": 8.0, "baseline_load_p95_ms": 10.0, "maximum_inclusive_ms": 12.5,
				"candidate_pool_p05": 8.0, "baseline_pool_p05": 8.0, "pool_minimum_inclusive": 7.2,
			}},
			"G5_stability":               {Passed: true, Details: map[string]any{"all_replicates_clean": true}},
			"G6_resources":               {Passed: true, Details: map[string]any{"all_replicates_within_limits": true}},
			"synthetic-private-gate-key": {Passed: false, Details: map[string]any{}},
		},
		Diagnostics: map[string]any{
			"seed": "synthetic-private-seed", "patch": "synthetic-private-patch", "review_decision": "synthetic-private-review",
			"replicate_count": 3.0, "request_timeout_ms": 120000.0, "takeover_margin": .12,
			"baseline":  map[string]any{"raw_score": 100.0, "request_count": 1000.0, "received_bytes": 10000.0, "path": "synthetic-private-path"},
			"candidate": map[string]any{"raw_score": 90.0, "request_count": 1100.0, "received_bytes": 11000.0, "findproviders_load_p95_ms": 8.0},
			"replicates": []any{map[string]any{
				"evaluation_id": "synthetic-private-evaluation", "raw_score": 90.0, "request_count": 1100.0,
				"stability_findings": []any{"synthetic-private-finding"}, "resource_findings": []any{"synthetic-private-finding"},
			}},
		},
	}
}

// Per-gate allowlists keep actionable counts and bounds without forwarding
// attacker-selected keys or free-form evaluator evidence.
func TestCompetitionPrivateScorePreservesReviewedGateDiagnostics(t *testing.T) {
	stored := privateScoreFeedbackFixture()
	before, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	feedback := privateCompletedScore(stored)
	if feedback == nil || validateScore(feedback) != nil || feedback.Placeable || len(feedback.Gates) != 6 {
		t.Fatalf("private completed score = %#v", feedback)
	}
	for _, field := range []string{"candidate_requests", "request_min", "request_max", "candidate_bytes", "bytes_min", "bytes_max"} {
		if feedback.Gates["G2_volume"].Details[field] != stored.Gates["G2_volume"].Details[field] {
			t.Errorf("G2 feedback lost %s", field)
		}
	}
	if feedback.Gates["G2_volume"].Passed || feedback.Gates["G5_stability"].Details["all_replicates_clean"] != true ||
		feedback.Gates["G6_resources"].Details["all_replicates_within_limits"] != true || feedback.Significance == nil {
		t.Fatalf("private gate reasons or significance = %#v", feedback)
	}
	encoded, err := json.Marshal(feedback)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("synthetic-private-")) || bytes.Contains(encoded, []byte("evaluation_id")) || bytes.Contains(encoded, []byte("review_decision")) {
		t.Fatalf("private evidence escaped reviewed projection: %s", encoded)
	}
	if !bytes.Contains(encoded, []byte("findproviders_load_p95_ms")) || !bytes.Contains(encoded, []byte("replicates")) {
		t.Fatalf("safe candidate summary/replicate statistics missing: %s", encoded)
	}
	after, err := json.Marshal(stored)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("private projection mutated stored score evidence")
	}
	var decoded ScoreResult
	if err := json.Unmarshal(encoded, &decoded); err != nil || !privateScoreProjectionMatches(&decoded) {
		t.Fatalf("projection is not stable across durable JSON: %v", err)
	}
}

// A known field name never upgrades strings, nested objects, or non-finite
// values into safe diagnostics; malformed required score fields fail closed.
func TestCompetitionPrivateScoreRejectsUnsafeDiagnosticValues(t *testing.T) {
	stored := privateScoreFeedbackFixture()
	stored.Gates["G2_volume"].Details["candidate_requests"] = "synthetic-private-count"
	stored.Gates["G2_volume"].Details["request_max"] = math.Inf(1)
	stored.Gates["G4_matchmaking"].Details["all_pools_nonempty"] = map[string]any{"seed": "synthetic-private-seed"}
	stored.Diagnostics["candidate"].(map[string]any)["raw_score"] = "synthetic-private-score"
	feedback := privateCompletedScore(stored)
	if feedback == nil {
		t.Fatal("unsafe optional diagnostics suppressed valid typed score")
	}
	for _, field := range []string{"candidate_requests", "request_max"} {
		if _, ok := feedback.Gates["G2_volume"].Details[field]; ok {
			t.Errorf("unsafe G2 %s survived projection", field)
		}
	}
	if _, ok := feedback.Gates["G4_matchmaking"].Details["all_pools_nonempty"]; ok {
		t.Fatal("nested object survived a boolean gate field")
	}
	if _, err := json.Marshal(feedback); err != nil {
		t.Fatalf("optional non-finite field contaminated response: %v", err)
	}
	stored.Significance = nil
	if privateCompletedScore(stored) != nil {
		t.Fatal("invalid required score contract crossed private feedback boundary")
	}
}

// Prospectively versioned G2 failures are fixed reviewed identifiers, not
// arbitrary scorer text. Their message/readiness detail remains embargoed.
func TestCompetitionPrivateScoreG2FailureSummaryRemainsMessageFree(t *testing.T) {
	for _, code := range []string{"invalid_g2_volume_policy", "g2_volume_policy_mismatch"} {
		stored := &CompetitionError{Kind: "infrastructure", Code: code, Message: "synthetic-private-policy-detail", Retriable: true,
			Readiness: &ReadinessResult{Checks: map[string]bool{"synthetic-private-check": false}}}
		failure := scoreJobEvaluationFailure("failed", stored)
		if failure == nil || failure.Kind != "infrastructure" || failure.Code != code || failure.Retriable {
			t.Fatalf("reviewed policy failure summary = %#v", failure)
		}
		encoded, err := json.Marshal(failure)
		if err != nil || bytes.Contains(encoded, []byte("synthetic-private-")) || !stored.Retriable {
			t.Fatalf("policy summary disclosed or rewrote private details: %s %v", encoded, err)
		}
		if scoreJobEvaluationFailure("running", stored) != nil || scoreJobEvaluationFailure("failed", &CompetitionError{Kind: "submission", Code: code}) != nil {
			t.Fatal("reviewed failure code escaped its exact lifecycle/kind pair")
		}
	}
	if scoreJobEvaluationFailure("failed", &CompetitionError{Kind: "infrastructure", Code: "synthetic_private_dynamic_code"}) != nil {
		t.Fatal("unreviewed policy failure code crossed the message-free boundary")
	}
}

// Nil maps, typed nil values, scalar summaries and malformed replicate entries
// are optional omissions, never panics or a way around the reviewed allowlist.
func TestCompetitionPrivateScoreNilAndMalformedDiagnostics(t *testing.T) {
	if privateCompletedScore(nil) != nil || privateScoreProjectionMatches(nil) {
		t.Fatal("nil score became private feedback")
	}
	for _, diagnostics := range []map[string]any{
		nil,
		{},
		{"baseline": nil, "candidate": "synthetic-private-summary", "replicates": "synthetic-private-replicates"},
		{"baseline": map[string]any(nil), "candidate": map[string]string{"raw_score": "synthetic-private-value"}},
		{"replicates": []any{nil, "synthetic-private-replicate", map[string]any(nil), map[string]any{"raw_score": 80.0}}},
	} {
		stored := privateScoreFeedbackFixture()
		stored.Diagnostics = diagnostics
		feedback := privateCompletedScore(stored)
		if feedback == nil || validateScore(feedback) != nil || !privateScoreProjectionMatches(feedback) {
			t.Fatalf("optional malformed diagnostics broke private projection: %#v", feedback)
		}
		encoded, err := json.Marshal(feedback)
		if err != nil || bytes.Contains(encoded, []byte("synthetic-private-")) {
			t.Fatalf("malformed diagnostics escaped: %s %v", encoded, err)
		}
	}
	stored := privateScoreFeedbackFixture()
	stored.Gates = nil
	if privateCompletedScore(stored) != nil {
		t.Fatal("missing required gate map became private feedback")
	}
	stored.Gates = map[string]Gate{"G2_volume": {Details: nil}}
	if privateCompletedScore(stored) != nil {
		t.Fatal("malformed required gate details became private feedback")
	}
}

// The shared service path keeps current-round policy and public decision mode
// immutable while making only completed production feedback privately readable.
func TestCompetitionPrivateScoreV1AndV2ProductionPolling(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		settings := validSettings()
		if v2 {
			settings = automationApiSettings()
		}
		policy, err := policySnapshot(settings)
		if err != nil {
			t.Fatal(err)
		}
		job := &queuedJob{
			ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: server.NewId(), State: "succeeded", Score: privateScoreFeedbackFixture()},
			Round:          roundRecord{RoundResult: RoundResult{RevealAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}, PolicyJson: policy},
		}
		job.Round.RoundId = job.RoundId
		store := &fakeStore{getJob: job}
		service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
		principal := &Principal{Id: "synthetic-credential-owner", Role: "submitter"}
		result, status, evalError := service.getScoreWithDecisionContract(context.Background(), job.JobId, principal, v2)
		if evalError != nil || status != http.StatusOK || result.State != "completed" || result.EvaluationStatus != "completed" ||
			result.Score == nil || !privateScoreProjectionMatches(result.Score) || result.EvalError != nil || result.EvaluationFailure != nil {
			t.Fatalf("v2=%t completed owner feedback = %#v %d %v", v2, result, status, evalError)
		}
		if !bytes.Equal(policy, job.Round.PolicyJson) || job.Round.FinalizedAt != nil || job.Round.AutomationPublicationAt != nil {
			t.Fatal("owner feedback rewrote frozen policy or published the round")
		}
		for _, state := range []string{"queued", "running", "failed", "canceled"} {
			job.State = state
			result, status, evalError = service.getScoreWithDecisionContract(context.Background(), job.JobId, principal, v2)
			if evalError != nil || status != http.StatusOK || result.Score != nil {
				t.Fatalf("v2=%t non-success %s exposed stale score: %#v %d %v", v2, state, result, status, evalError)
			}
		}
		job.State = "succeeded"
		job.Staging, job.Round.Staging = true, true
		if v2 {
			// A separate valid staging fixture verifies its immutable mode.
			_, stagingStore, stagingService := newAutomationStagingApiFixture(t)
			stagingJob := *stagingStore.enqueueJob
			stagingJob.State, stagingJob.Score = "succeeded", privateScoreFeedbackFixture()
			stagingStore.fakeStore.getJob = &stagingJob
			result, status, evalError = stagingService.GetScoreV2(context.Background(), stagingJob.JobId, principal)
		} else {
			result, status, evalError = service.GetScore(context.Background(), job.JobId, principal)
		}
		if evalError != nil || status != http.StatusOK || result.Score != nil || result.State != "completed" {
			t.Fatalf("v2=%t staging embargo changed: %#v %d %v", v2, result, status, evalError)
		}
	}
}

// V2 owner feedback neither seals an open round nor releases its result,
// decisions, workload or reward feed. The real credential-owner join still gates it.
func TestCompetitionPrivateScoreV2DurableOwnerDoesNotPublishRound(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		job := addCompetitionAutomationJob(t, fixture, 90)
		before, err := fixture.store.GetRound(ctx, fixture.settings, job.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO competition_job_principal(job_id,principal_id,first_seen_at) VALUES($1,$2,$3)`, job.JobId, "synthetic-private-owner", fixture.now))
		}, server.OptReadWrite())
		service := newServiceWithImageDigest(fixture.settings, fixture.store, testApiImageDigest(), nil)
		owner := &Principal{Id: "synthetic-private-owner", Role: "submitter"}
		result, status, evalError := service.GetScoreV2(ctx, job.JobId, owner)
		if evalError != nil || status != http.StatusOK || result.Score == nil || result.State != "completed" ||
			result.DecisionMode != model.CompetitionDecisionModeV2 || !privateScoreProjectionMatches(result.Score) {
			t.Fatalf("durable v2 private feedback = %#v %d %v", result, status, evalError)
		}
		if result, status, evalError := service.GetScoreV2(ctx, job.JobId, &Principal{Id: "synthetic-other-owner", Role: "submitter"}); result != nil || status != http.StatusNotFound || evalError == nil {
			t.Fatalf("v2 cross-owner score = %#v %d %v", result, status, evalError)
		}
		if result, status, evalError := service.AutomationResult(ctx, job.RoundId); result != nil || status != http.StatusConflict || evalError == nil || evalError.Code != "round_not_published" {
			t.Fatalf("owner feedback published v2 result/decisions: %#v %d %v", result, status, evalError)
		}
		if events, err := fixture.store.RewardEvents(ctx, fixture.settings, 0, 10); err != nil || len(events.Events) != 0 {
			t.Fatalf("owner feedback published a reward event: %#v %v", events, err)
		}
		if workload, _, status, evalError := service.GetRoundWorkload(ctx, job.RoundId); workload != nil || status != http.StatusConflict || evalError == nil {
			t.Fatalf("owner feedback revealed v2 workload: %q %d %v", workload, status, evalError)
		}
		stored, err := fixture.store.GetRound(ctx, fixture.settings, job.RoundId)
		if err != nil || !bytes.Equal(stored.PolicyJson, before.PolicyJson) || stored.AutomationPublicationAt != nil || stored.AutomationSealedAt != nil || stored.WinnerJobId != nil {
			t.Fatalf("private feedback mutated v2 policy or public seal/winner: %#v %v", stored, err)
		}
	})
}

// A broken store contract cannot turn a missing private row into a nil-pointer
// panic or an unguarded score response after authorization succeeds.
func TestCompetitionPrivateScoreMissingStoredJobFailsClosed(t *testing.T) {
	service := newServiceWithImageDigest(validSettings(), &fakeStore{}, testApiImageDigest(), nil)
	result, status, evalError := service.GetScore(context.Background(), server.NewId(), &Principal{Id: "synthetic-owner", Role: "submitter"})
	if result != nil || status != http.StatusServiceUnavailable || evalError == nil || evalError.Code != "storage_unavailable" {
		t.Fatalf("missing stored job = %#v %d %v", result, status, evalError)
	}
}

// A durable public hold never re-exposes the full document from an earlier
// publication. Private owner measurements remain distinct from public release.
func TestCompetitionPrivateScoreHeldV2ResultStaysSanitized(t *testing.T) {
	settings := automationApiSettings()
	policy, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	publicationAt := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	job := &queuedJob{
		ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: server.NewId(), State: "succeeded", Score: privateScoreFeedbackFixture()},
		Round:          roundRecord{CompetitionId: settings.CompetitionId, PolicyJson: policy, AutomationPublicationAt: &publicationAt},
	}
	store := &fakeStore{getJob: job, holdStatus: competitionAutomationHoldStatus{Evaluation: true}}
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	result, status, evalError := service.GetScoreV2(context.Background(), job.JobId, &Principal{Id: "synthetic-owner", Role: "submitter"})
	if evalError != nil || status != http.StatusOK || result.State != "completed" || result.Score == nil || !privateScoreProjectionMatches(result.Score) {
		t.Fatalf("held private score = %#v %d %v", result, status, evalError)
	}
	store.holdStatusErr = errors.New("synthetic hold storage failure")
	if result, status, evalError := service.GetScoreV2(context.Background(), job.JobId, &Principal{Id: "synthetic-owner", Role: "submitter"}); result != nil || status != http.StatusServiceUnavailable || evalError == nil {
		t.Fatalf("hold read failure became private/public release: %#v %d %v", result, status, evalError)
	}
}

// The store double checks the same credential-owner association as the durable
// query, while the transport fixture exercises real auth and path dispatch.
type privateScoreOwnerStore struct {
	*fakeStore
	owner string
	calls int
}

// Unknown and cross-owner jobs have the same not-found response.
func (self *privateScoreOwnerStore) GetJob(_ context.Context, _ *Settings, jobId server.Id, principal *Principal) (*queuedJob, error) {
	self.calls++
	if !competitionScorePrincipalAllowed(principal, false) || jobId != self.getJob.JobId || principal.Role != "operator" && principal.Id != self.owner {
		return nil, ErrNotFound
	}
	return self.getJob, nil
}

// An authenticated owner response must not be reusable by anonymous readers,
// another credential, control-plane roles, or an operator on a v2 route.
func TestCompetitionPrivateScoreHttpAuthAndNoStore(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		settings := validSettings()
		if v2 {
			settings = automationApiSettings()
		}
		otherHash := sha256.Sum256([]byte("synthetic-other-secret"))
		reviewerHash := sha256.Sum256([]byte("synthetic-reviewer-secret"))
		settings.Tokens = append(settings.Tokens,
			Token{Name: "synthetic-other-owner", Role: "submitter", Sha256: hex.EncodeToString(otherHash[:])},
			Token{Name: "synthetic-readonly-reviewer", Role: competitionReviewerRole, Sha256: hex.EncodeToString(reviewerHash[:]),
				Environment: "synthetic-environment", CompetitionId: settings.CompetitionId, SeasonId: "synthetic-season"})
		policy, err := policySnapshot(settings)
		if err != nil {
			t.Fatal(err)
		}
		job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: server.NewId(), State: "succeeded", Score: privateScoreFeedbackFixture()},
			Round: roundRecord{CompetitionId: settings.CompetitionId, PolicyJson: policy}}
		store := &privateScoreOwnerStore{fakeStore: &fakeStore{getJob: job, round: &job.Round}, owner: "apex"}
		service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
		previousService := defaultService
		defaultService = func() *Service { return service }
		func() {
			defer func() { defaultService = previousService }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			endpoint, handler := "/competition/score/", GetScoreHandler
			if v2 {
				endpoint, handler = "/competition/v2/score/", CompetitionV2GetScoreHandler
			}
			transport := router.NewRouter(ctx, []*router.Route{router.NewRoute(http.MethodGet, endpoint+"([^/]+)", handler)})
			for _, c := range []struct {
				secret string
				status int
				score  bool
			}{
				{secret: "submit-secret", status: http.StatusOK, score: true},
				{status: http.StatusUnauthorized},
				{secret: "synthetic-other-secret", status: http.StatusNotFound},
				{secret: "synthetic-reviewer-secret", status: http.StatusUnauthorized},
				{secret: "synthetic-invalid-secret", status: http.StatusUnauthorized},
			} {
				request := httptest.NewRequest(http.MethodGet, endpoint+job.JobId.String(), nil)
				if c.secret != "" {
					request.Header.Set("Authorization", "Bearer "+c.secret)
				}
				response := httptest.NewRecorder()
				transport.ServeHTTP(response, request)
				var wire map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
					t.Fatal(err)
				}
				_, hasScore := wire["score"]
				if response.Code != c.status || hasScore != c.score || response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "synthetic-private-") {
					t.Fatalf("v2=%t auth response: %d %v %s", v2, response.Code, response.Header(), response.Body.String())
				}
			}
			request := httptest.NewRequest(http.MethodGet, endpoint+job.JobId.String(), nil)
			request.Header.Set("Authorization", "Bearer operator-secret")
			response := httptest.NewRecorder()
			transport.ServeHTTP(response, request)
			if v2 && response.Code != http.StatusForbidden || !v2 && response.Code != http.StatusOK {
				t.Fatalf("v2=%t operator authority = %d", v2, response.Code)
			}
		}()
		calls := store.calls
		for _, principal := range []*Principal{nil, {Id: "synthetic-readonly", Role: competitionReviewerRole}, {Role: "submitter"}} {
			if result, status, evalError := service.getScoreWithDecisionContract(context.Background(), job.JobId, principal, v2); result != nil || status != http.StatusForbidden || evalError == nil {
				t.Fatalf("v2=%t direct unauthorized caller = %#v %d %v", v2, result, status, evalError)
			}
		}
		if store.calls != calls {
			t.Fatal("unauthorized direct caller reached private job storage")
		}
	}
}

// The real PostgreSQL owner join, score completion and public workload and
// leaderboard projections remain composed; no queue, worker or sleeps are used.
func TestCompetitionPrivateScoreDurableOwnershipAndPublicEmbargo(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionRankingFixture(t, false)
		job := fixture.addJob(t)
		outcome := testSharedBaselineOutcome(t, fixture.settings, job, EvaluationOutcome{Score: privateScoreFeedbackFixture()})
		if retry, err := fixture.store.Complete(ctx, fixture.settings, "ranking-worker", job.JobId, outcome); err != nil || retry {
			t.Fatalf("complete private fixture: retry=%t error=%v", retry, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO competition_job_principal(job_id,principal_id,first_seen_at) VALUES($1,$2,$3)`, job.JobId, "synthetic-cache-coowner", fixture.now))
		}, server.OptReadWrite())
		service := newServiceWithImageDigest(fixture.settings, fixture.store, testApiImageDigest(), nil)
		for _, owner := range []string{"synthetic-ranking-submitter", "synthetic-cache-coowner"} {
			result, status, evalError := service.GetScore(ctx, job.JobId, &Principal{Id: owner, Role: "submitter"})
			if evalError != nil || status != http.StatusOK || result.Score == nil || !privateScoreProjectionMatches(result.Score) || result.State != "completed" {
				t.Fatalf("durable owner %s = %#v %d %v", owner, result, status, evalError)
			}
		}
		for _, id := range []server.Id{job.JobId, server.NewId()} {
			result, status, evalError := service.GetScore(ctx, id, &Principal{Id: "synthetic-other-owner", Role: "submitter"})
			if result != nil || status != http.StatusNotFound || evalError == nil || evalError.Code != "job_not_found" {
				t.Fatalf("cross-owner/unknown job = %#v %d %v", result, status, evalError)
			}
		}
		for _, principal := range []*Principal{nil, {Id: "synthetic-readonly", Role: competitionReviewerRole}} {
			if result, err := fixture.store.GetJob(ctx, fixture.settings, job.JobId, principal); result != nil || !errors.Is(err, ErrNotFound) {
				t.Fatalf("durable unauthorized principal = %#v %v", result, err)
			}
		}
		operator, status, evalError := service.GetScore(ctx, job.JobId, &Principal{Id: "synthetic-operator", Role: "operator"})
		if evalError != nil || status != http.StatusOK || operator.Score == nil || operator.State != "succeeded" || operator.Score.Diagnostics["seed"] == nil {
			t.Fatalf("legacy operator inspection changed: %#v %d %v", operator, status, evalError)
		}
		boards, evalError := service.Leaderboards(ctx, false)
		if evalError != nil || len(boards.Epochs) != 0 {
			t.Fatalf("private score entered public leaderboard: %#v %v", boards, evalError)
		}
		if workload, _, status, evalError := service.GetRoundWorkload(ctx, job.RoundId); workload != nil || status != http.StatusConflict || evalError == nil {
			t.Fatalf("private score revealed workload: %q %d %v", workload, status, evalError)
		}
		view, evalError := service.roundView(fixture.round)
		if evalError != nil || view.RevealedSeed != nil || view.ProvidersUrl != "" || view.WinnerJobId != nil || view.FinalizedAt != nil {
			t.Fatalf("private score revealed seed/winner/finalization: %#v %v", view, evalError)
		}
	})
}

// The constructor owns a new private child directory. Existing Go TempDir
// permissions vary by toolchain and umask; public parents never waive the
// state directory's exact private boundary.
func TestCompetitionPrivateScoreStateDirectoryPermissions(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "private-state")
	if store, err := NewApexAdapterFileStore(directory); err != nil || store == nil {
		t.Fatalf("new private state: %v", err)
	}
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("new state directory is not private: %v %v", info, err)
	}
	for _, c := range []struct {
		name string
		mode os.FileMode
	}{
		{name: "world-readable-tempdir", mode: 0o755},
		{name: "group-readable-state", mode: 0o750},
		{name: "world-accessible-state", mode: 0o707},
	} {
		path := filepath.Join(parent, c.name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		// Explicit permissions reproduce the unsafe fixture under any umask.
		if err := os.Chmod(path, c.mode); err != nil {
			t.Fatal(err)
		}
		if store, err := NewApexAdapterFileStore(path); err == nil || store != nil {
			t.Errorf("%s granted private state authority: %v", c.name, err)
		}
	}
}

// Adapter feedback persists separately from public outcome, winner and review
// state, survives reopening, and cannot change the measurement later published.
func TestCompetitionPrivateScoreApexRecordNeverPublishesOwnerFeedback(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		directory := filepath.Join(t.TempDir(), "private-state")
		store, err := NewApexAdapterFileStore(directory)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		roundId, jobId := server.NewId(), server.NewId()
		if _, err := store.BeginSubmission("synthetic-private-submission", strings.Repeat("a", 64), now); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RecordFee("synthetic-private-submission", "synthetic-fee-receipt", now); err != nil {
			t.Fatal(err)
		}
		round := RoundResult{RoundId: roundId}
		accepted := ScoreAcceptedResult{JobId: jobId, RoundId: roundId, PatchSha256: strings.Repeat("b", 64), State: "queued", StatusUrl: "/competition/score/" + jobId.String()}
		if v2 {
			round.DecisionMode, round.RequiredApiCapability = model.CompetitionDecisionModeV2, model.CompetitionAutomationCapability
			accepted.DecisionMode, accepted.RequiredApiCapability = round.DecisionMode, round.RequiredApiCapability
			accepted.StatusUrl = "/competition/v2/score/" + jobId.String()
		}
		if _, err := store.recordRound("synthetic-private-submission", round, "synthetic-competition", now); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RecordAdmission("synthetic-private-submission", accepted, now); err != nil {
			t.Fatal(err)
		}
		poll := ScoreJobResult{JobId: jobId, RoundId: roundId, PatchSha256: accepted.PatchSha256, State: "completed", EvaluationStatus: "completed", Score: privateCompletedScore(privateScoreFeedbackFixture()),
			DecisionMode: accepted.DecisionMode, RequiredApiCapability: accepted.RequiredApiCapability}
		record, err := store.RecordPoll("synthetic-private-submission", poll, now)
		if err != nil || record.PrivateScore == nil || record.Score != nil || record.Published || record.Winner || record.HonestyReview != "" {
			t.Fatalf("v2=%t private adapter poll = %#v %v", v2, record, err)
		}
		reopened, err := NewApexAdapterFileStore(directory)
		if err != nil {
			t.Fatal(err)
		}
		record, err = reopened.Get("synthetic-private-submission")
		if err != nil || record.PrivateScore == nil || record.Published || record.Score != nil {
			t.Fatalf("v2=%t reopened private feedback = %#v %v", v2, record, err)
		}
		olderPoll := poll
		olderPoll.Score = nil
		if !v2 {
			olderPoll.EvaluationStatus = ""
		}
		record, err = reopened.RecordPoll("synthetic-private-submission", olderPoll, now)
		if err != nil || record.PrivateScore == nil || record.Published || record.Score != nil || record.EvaluationStatus != "completed" {
			t.Fatalf("v2=%t older replica retracted private feedback: %#v %v", v2, record, err)
		}
		unsafe := poll
		unsafe.Score = privateScoreFeedbackFixture()
		if _, err := reopened.RecordPoll("synthetic-private-submission", unsafe, now); err == nil {
			t.Fatal("adapter accepted arbitrary private evaluator diagnostics")
		}
		changed := poll
		changed.Score = privateCompletedScore(privateScoreFeedbackFixture())
		*changed.Score.RawScore = 89
		if _, err := reopened.RecordPoll("synthetic-private-submission", changed, now); err == nil {
			t.Fatal("adapter accepted a changed completed private score")
		}
		failed := poll
		failed.State, failed.EvaluationStatus, failed.Score = "completed", "failed", nil
		if _, err := reopened.RecordPoll("synthetic-private-submission", failed, now); err == nil {
			t.Fatal("adapter converted a completed private score into failure")
		}
		if v2 {
			published := poll
			published.State, published.Score = "succeeded", privateScoreFeedbackFixture()
			record, err = reopened.RecordPoll("synthetic-private-submission", published, now)
			if err != nil || !record.Published || record.Score == nil || record.PrivateScore != nil || record.Winner || record.HonestyReview != "" {
				t.Fatalf("public v2 score acquired legacy approval or retained private cache: %#v %v", record, err)
			}
		} else {
			if err := reopened.ReconcileLeaderboard(SeasonLeaderboardResult{Epochs: []LeaderboardResult{{
				RoundId: roundId, Status: "finalized", FinalizedAt: now,
				Entries: []LeaderboardEntry{{Rank: 1, JobId: jobId, PatchSha256: accepted.PatchSha256,
					HonestyReview: "not_reviewed", Score: *privateScoreFeedbackFixture()}},
			}}}, now); err != nil {
				t.Fatal(err)
			}
			record, err = reopened.Get("synthetic-private-submission")
			if err != nil || !record.Published || record.Score == nil || record.PrivateScore != nil || record.Winner || record.HonestyReview != "not_reviewed" {
				t.Fatalf("finalized v1 score changed approval or retained private cache: %#v %v", record, err)
			}
		}
	}
}

// Successful pre-upgrade completions refresh automatically through rolling
// replicas. The ordinary FIFO and refresh lane alternate; a durable interval
// prevents old nil responses or an adapter restart from creating a hot loop.
func TestCompetitionPrivateScoreApexRefreshesCompletedAdmissionsAcrossRollingReplicas(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		directory := filepath.Join(t.TempDir(), "private-state")
		store, err := NewApexAdapterFileStore(directory)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		refreshEvery := time.Hour
		round := RoundResult{RoundId: server.NewId()}
		endpoint := "/competition/score/"
		if v2 {
			round.DecisionMode, round.RequiredApiCapability = model.CompetitionDecisionModeV2, model.CompetitionAutomationCapability
			endpoint = "/competition/v2/score/"
		}
		names := []string{"synthetic-completed-a", "synthetic-completed-b", "synthetic-active-a", "synthetic-active-b"}
		polls := map[string]ScoreJobResult{}
		var sharedPoll ScoreJobResult
		for i, name := range names {
			patchDigest := sha256.Sum256([]byte(name))
			patchSha256 := hex.EncodeToString(patchDigest[:])
			jobId := server.NewId()
			if _, err := store.BeginSubmission(name, patchSha256, now); err != nil {
				t.Fatal(err)
			}
			if _, err := store.recordRound(name, round, "synthetic-competition", now); err != nil {
				t.Fatal(err)
			}
			if _, err := store.RecordFee(name, "synthetic-fee-"+name, now); err != nil {
				t.Fatal(err)
			}
			if _, err := store.RecordAdmission(name, ScoreAcceptedResult{JobId: jobId, RoundId: round.RoundId, PatchSha256: patchSha256,
				State: "queued", StatusUrl: endpoint + jobId.String(), DecisionMode: round.DecisionMode, RequiredApiCapability: round.RequiredApiCapability}, now); err != nil {
				t.Fatal(err)
			}
			poll := ScoreJobResult{JobId: jobId, RoundId: round.RoundId, PatchSha256: patchSha256, State: "completed", EvaluationStatus: "completed",
				DecisionMode: round.DecisionMode, RequiredApiCapability: round.RequiredApiCapability}
			if i < 2 {
				if _, err := store.RecordPoll(name, poll, now); err != nil {
					t.Fatal(err)
				}
			}
			if i == 0 {
				sharedPoll = poll
			}
			polls[endpoint+jobId.String()] = poll
		}
		alias := "synthetic-cached-completed-a-alias"
		preparePrivateScoreAssociation(t, store, alias, round, now)
		if _, err := store.RecordAdmission(alias, ScoreAcceptedResult{JobId: sharedPoll.JobId, RoundId: sharedPoll.RoundId,
			PatchSha256: sharedPoll.PatchSha256, State: "completed", CacheHit: true, StatusUrl: endpoint + sharedPoll.JobId.String(),
			DecisionMode: sharedPoll.DecisionMode, RequiredApiCapability: sharedPoll.RequiredApiCapability}, now); err != nil {
			t.Fatal(err)
		}
		var stateLock sync.Mutex
		calls := []string{}
		counts := map[string]int{}
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-private-owner-token" {
				t.Error("private refresh changed method or credential boundary")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			poll, ok := polls[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			stateLock.Lock()
			calls = append(calls, poll.PatchSha256)
			counts[poll.PatchSha256]++
			attempt := counts[poll.PatchSha256]
			stateLock.Unlock()
			firstDigest := sha256.Sum256([]byte(names[0]))
			if poll.PatchSha256 != hex.EncodeToString(firstDigest[:]) || attempt > 1 {
				poll.Score = privateCompletedScore(privateScoreFeedbackFixture())
			} else if !v2 {
				// A genuinely old v1 replica omits the additive lifecycle field.
				writeCompetitionJson(w, http.StatusOK, map[string]any{
					"job_id": poll.JobId, "round_id": poll.RoundId, "patch_sha256": poll.PatchSha256,
					"state": poll.State, "staging": poll.Staging,
				})
				return
			}
			writeCompetitionJson(w, http.StatusOK, poll)
		}))
		options := ApexAdapterOptions{HttpClient: api.Client(), MaxAttempts: 1, Now: func() time.Time { return now },
			V2CompetitionId: "synthetic-competition", PrivateScoreRefreshEvery: refreshEvery}
		adapter, err := NewApexAdapter(api.URL, "synthetic-private-owner-token", store, &apexConformanceFeeCollector{}, options)
		if err != nil {
			api.Close()
			t.Fatal(err)
		}
		for _, name := range []string{names[2], names[0], names[3], names[1]} {
			record, err := adapter.PollNext(context.Background())
			if err != nil || record == nil || record.SubmissionId != name || record.Published || record.Score != nil || record.Winner || record.HonestyReview != "" {
				api.Close()
				t.Fatalf("v2=%t FIFO/refresh fairness for %s: %#v %v", v2, name, record, err)
			}
		}
		reopened, err := NewApexAdapterFileStore(directory)
		if err != nil {
			api.Close()
			t.Fatal(err)
		}
		associated, err := reopened.Get(alias)
		if err != nil || associated.PrivateScoreRefreshAt == nil || associated.PrivateScore != nil || associated.Published {
			api.Close()
			t.Fatalf("v2=%t cached association bypassed shared refresh backoff: %#v %v", v2, associated, err)
		}
		adapter, err = NewApexAdapter(api.URL, "synthetic-private-owner-token", reopened, &apexConformanceFeeCollector{}, options)
		if err != nil {
			api.Close()
			t.Fatal(err)
		}
		now = now.Add(refreshEvery - time.Second)
		if record, err := adapter.PollNext(context.Background()); err != nil || record != nil {
			api.Close()
			t.Fatalf("v2=%t old replica backoff lost across restart: %#v %v", v2, record, err)
		}
		now = now.Add(time.Second)
		record, err := adapter.PollNext(context.Background())
		if err != nil || record == nil || record.SubmissionId != names[0] || record.PrivateScore == nil || record.PrivateScoreRefreshAt != nil || record.Published {
			api.Close()
			t.Fatalf("v2=%t due rolling private feedback = %#v %v", v2, record, err)
		}
		associated, err = reopened.Get(alias)
		if err != nil || associated.PrivateScore == nil || associated.PrivateScoreRefreshAt != nil || associated.SubmissionId != alias || associated.Published {
			api.Close()
			t.Fatalf("v2=%t shared due feedback lost its miner association: %#v %v", v2, associated, err)
		}
		if record, err := adapter.PollNext(context.Background()); err != nil || record != nil {
			api.Close()
			t.Fatalf("v2=%t completed private feedback kept routine polling: %#v %v", v2, record, err)
		}
		api.Close()
		expected := []string{}
		for _, name := range []string{names[2], names[0], names[3], names[1], names[0]} {
			digest := sha256.Sum256([]byte(name))
			expected = append(expected, hex.EncodeToString(digest[:]))
		}
		stateLock.Lock()
		observed := strings.Join(calls, ",")
		stateLock.Unlock()
		if observed != strings.Join(expected, ",") {
			t.Fatalf("v2=%t refresh request order/count = %s, want %v", v2, observed, expected)
		}
	}
}

// Known scoring success and ambiguous cached completions need an owner probe;
// the latter remain unknown rather than silently becoming scored successes.
func TestCompetitionPrivateScoreApexRefreshEligibility(t *testing.T) {
	eligible := ApexAdapterRecord{JobId: server.NewId(), State: "completed", EvaluationStatus: "completed"}
	if !apexPrivateScoreRefreshEligible(eligible) {
		t.Fatal("completed production score is not refresh eligible")
	}
	ambiguous := ApexAdapterRecord{JobId: eligible.JobId, State: "completed"}
	if !apexPrivateScoreRefreshEligible(ambiguous) || ambiguous.EvaluationStatus != "" {
		t.Fatal("ambiguous cached admission lost its safe probe or invented success")
	}
	for _, excluded := range []ApexAdapterRecord{
		{State: "completed", EvaluationStatus: "completed"},
		{JobId: eligible.JobId, State: "completed", EvaluationStatus: "failed"},
		{JobId: eligible.JobId, State: "canceled", EvaluationStatus: "canceled"},
		{JobId: eligible.JobId, State: "completed", EvaluationStatus: "completed", Staging: true},
		{JobId: eligible.JobId, State: "completed", EvaluationStatus: "completed", Published: true},
		{JobId: eligible.JobId, State: "completed", EvaluationStatus: "completed", PrivateScore: privateCompletedScore(privateScoreFeedbackFixture())},
		{JobId: eligible.JobId, State: "completed", EvaluationStatus: "completed", EvaluationFailure: &EvaluationFailure{Kind: "submission", Code: "run_unstable"}},
	} {
		if apexPrivateScoreRefreshEligible(excluded) {
			t.Errorf("terminal/unknown/non-production record became refresh eligible: %#v", excluded)
		}
	}
}

// A cache-hit admission contains only an ambiguous completed marker. Automatic
// owner polling learns success or safe failure without inferring either from
// admission, and a failed probe stops instead of acquiring a private score.
func TestCompetitionPrivateScoreApexRefreshesCachedCompletionsWithoutInventingSuccess(t *testing.T) {
	store, err := NewApexAdapterFileStore(filepath.Join(t.TempDir(), "private-state"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	roundId := server.NewId()
	names := []string{"synthetic-cached-score", "synthetic-cached-failure"}
	polls := map[string]ScoreJobResult{}
	for i, name := range names {
		jobId := server.NewId()
		digest := sha256.Sum256([]byte(name))
		patchSha256 := hex.EncodeToString(digest[:])
		if _, err := store.BeginSubmission(name, patchSha256, now); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RecordRound(name, roundId, false, now); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RecordFee(name, "synthetic-cache-fee-"+name, now); err != nil {
			t.Fatal(err)
		}
		endpoint := "/competition/score/" + jobId.String()
		admitted, err := store.RecordAdmission(name, ScoreAcceptedResult{JobId: jobId, RoundId: roundId, PatchSha256: patchSha256, State: "completed", CacheHit: true, StatusUrl: endpoint}, now)
		if err != nil || admitted.EvaluationStatus != "" || admitted.PrivateScore != nil || admitted.Published {
			t.Fatalf("cache-hit admission invented an outcome: %#v %v", admitted, err)
		}
		poll := ScoreJobResult{JobId: jobId, RoundId: roundId, PatchSha256: patchSha256, State: "completed", EvaluationStatus: "completed"}
		if i == 0 {
			poll.Score = privateCompletedScore(privateScoreFeedbackFixture())
		} else {
			poll.EvaluationStatus = "failed"
			poll.EvaluationFailure = &EvaluationFailure{Kind: "submission", Code: "candidate_build_failed"}
		}
		polls[endpoint] = poll
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-cache-owner-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		poll, ok := polls[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeCompetitionJson(w, http.StatusOK, poll)
	}))
	defer api.Close()
	adapter, err := NewApexAdapter(api.URL, "synthetic-cache-owner-token", store, &apexConformanceFeeCollector{}, ApexAdapterOptions{
		HttpClient: api.Client(), MaxAttempts: 1, Now: func() time.Time { return now }, PrivateScoreRefreshEvery: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		record, err := adapter.PollNext(context.Background())
		if err != nil || record == nil || record.SubmissionId != name || record.Published || record.Score != nil || record.PrivateScoreRefreshAt != nil {
			t.Fatalf("cache-hit automatic owner poll = %#v %v", record, err)
		}
		if i == 0 && (record.PrivateScore == nil || record.EvaluationStatus != "completed") ||
			i == 1 && (record.PrivateScore != nil || record.EvaluationStatus != "failed" || record.EvaluationFailure == nil) {
			t.Fatalf("cached score/failure identity changed: %#v", record)
		}
	}
	if record, err := adapter.PollNext(context.Background()); err != nil || record != nil {
		t.Fatalf("terminal cache outcomes kept routine polling: %#v %v", record, err)
	}
}

// A failed refresh reserves its next attempt before HTTP, so neither immediate
// caller retries nor a restart repeatedly hammer an unavailable API replica.
func TestCompetitionPrivateScoreApexRefreshBacksOffTransportFailures(t *testing.T) {
	store, err := NewApexAdapterFileStore(filepath.Join(t.TempDir(), "private-state"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	jobId, roundId := server.NewId(), server.NewId()
	name, patchSha256 := "synthetic-refresh-transport", strings.Repeat("a", 64)
	if _, err := store.BeginSubmission(name, patchSha256, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordRound(name, roundId, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordFee(name, "synthetic-refresh-receipt", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordAdmission(name, ScoreAcceptedResult{JobId: jobId, RoundId: roundId, PatchSha256: patchSha256, State: "queued", StatusUrl: "/competition/score/" + jobId.String()}, now); err != nil {
		t.Fatal(err)
	}
	poll := ScoreJobResult{JobId: jobId, RoundId: roundId, PatchSha256: patchSha256, State: "completed", EvaluationStatus: "completed"}
	if _, err := store.RecordPoll(name, poll, now); err != nil {
		t.Fatal(err)
	}
	var stateLock sync.Mutex
	calls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stateLock.Lock()
		calls++
		attempt := calls
		stateLock.Unlock()
		if attempt == 1 {
			writeCompetitionJson(w, http.StatusServiceUnavailable, infrastructureError("storage_unavailable", "synthetic unavailable replica"))
			return
		}
		completed := poll
		completed.Score = privateCompletedScore(privateScoreFeedbackFixture())
		writeCompetitionJson(w, http.StatusOK, completed)
	}))
	defer api.Close()
	options := ApexAdapterOptions{HttpClient: api.Client(), MaxAttempts: 1, Now: func() time.Time { return now }, PrivateScoreRefreshEvery: time.Minute}
	adapter, err := NewApexAdapter(api.URL, "synthetic-owner-token", store, &apexConformanceFeeCollector{}, options)
	if err != nil {
		t.Fatal(err)
	}
	if record, err := adapter.PollNext(context.Background()); err == nil || record != nil {
		t.Fatalf("failed refresh was reported as feedback: %#v %v", record, err)
	}
	if record, err := adapter.PollNext(context.Background()); err != nil || record != nil {
		t.Fatalf("failed refresh did not back off: %#v %v", record, err)
	}
	now = now.Add(options.PrivateScoreRefreshEvery)
	if record, err := adapter.PollNext(context.Background()); err != nil || record == nil || record.PrivateScore == nil || record.Published {
		t.Fatalf("refresh did not recover after bounded backoff: %#v %v", record, err)
	}
	stateLock.Lock()
	observedCalls := calls
	stateLock.Unlock()
	if observedCalls != 2 {
		t.Fatalf("refresh amplification = %d requests, want 2", observedCalls)
	}
	options.PrivateScoreRefreshEvery = -time.Second
	if _, err := NewApexAdapter(api.URL, "synthetic-owner-token", store, &apexConformanceFeeCollector{}, options); err == nil {
		t.Fatal("invalid refresh interval disabled default owner feedback")
	}
}
