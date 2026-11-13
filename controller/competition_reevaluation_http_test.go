// Real owned polling handlers preserve the canonical recovery marker without
// publishing private outcomes or exposing immutable operator audit payloads.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// Both existing transport contracts serialize the same owned recovery, keep
// unpublished outcomes/private errors sealed, and refuse unauthorized cache reuse.
func TestCompetitionReevaluationHttpPreservesOwnedReceiptAndPublicationEmbargo(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		func() {
			settings := validSettings()
			if v2 {
				settings = automationApiSettings()
			}
			controlEnvironment, controlSeasonId := "synthetic-control-env", "synthetic-control-season"
			if v2 {
				controlEnvironment, controlSeasonId = settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId
			}
			for _, token := range []struct {
				name, role, secret string
			}{
				{name: "synthetic-recovery-other", role: "submitter", secret: "synthetic-recovery-other-token"},
				{name: "synthetic-recovery-reviewer", role: competitionReviewerRole, secret: "synthetic-recovery-reviewer-token"},
				{name: "synthetic-recovery-scheduler", role: competitionSchedulerRole, secret: "synthetic-recovery-scheduler-token"},
			} {
				digest := sha256.Sum256([]byte(token.secret))
				settings.Tokens = append(settings.Tokens, Token{Name: token.name, Role: token.role, Sha256: hex.EncodeToString(digest[:]),
					Environment: controlEnvironment, CompetitionId: settings.CompetitionId, SeasonId: controlSeasonId})
			}
			policy, err := policySnapshot(settings)
			if err != nil {
				t.Fatal(err)
			}
			cycle := newCompetitionReevaluationCycleTestFixture(t)
			cycle.CompetitionId = settings.CompetitionId
			if v2 {
				cycle.Environment, cycle.SeasonId = settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId
			}
			cycle.RoundPolicySha256, err = CompetitionJobReevaluationSnapshotDigest(policy)
			if err != nil {
				t.Fatal(err)
			}
			cycleStartedAt := cycle.AuthorizedAt.Add(time.Second)
			cycle.StartedAt = &cycleStartedAt
			cycle.ReceiptSha256, err = model.CompetitionJobReevaluationCycleDigest(cycle)
			if err != nil || model.ValidateCompetitionJobReevaluationCycle(&cycle) != nil {
				t.Fatalf("canonical synthetic HTTP recovery marker: %v", err)
			}
			receipt := model.CompetitionJobReevaluationReceipt{CompetitionJobReevaluationCycle: cycle,
				RequestSha256: strings.Repeat("f", 64), AttemptOffset: 3}
			receiptBefore, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			originalStartedAt := cycle.AuthorizedAt.Add(-24 * time.Hour)
			completedAt := cycleStartedAt.Add(time.Minute)
			job := &queuedJob{
				ScoreJobResult: ScoreJobResult{
					JobId: cycle.JobId, RoundId: cycle.RoundId, PatchSha256: cycle.PatchSha256, State: "failed",
					StartedAt: &originalStartedAt, CompletedAt: &completedAt, Reevaluation: &receipt.CompetitionJobReevaluationCycle,
					EvalError: terminalInfrastructureError("artifact_archive_failed", "synthetic-private-current-recovery-error"),
				},
				EvaluationCycle: 1, AttemptCount: 4, CycleAttemptOffset: 3, CycleStartedAt: &cycleStartedAt,
				Round: roundRecord{RoundResult: RoundResult{RoundId: cycle.RoundId, Epoch: 1,
					RevealAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}, CompetitionId: settings.CompetitionId, PolicyJson: policy},
			}
			if v2 {
				publicationAt := job.Round.RevealAt
				job.Round.AutomationPublicationAt = &publicationAt
			}
			if roundPublished(&job.Round, time.Now().UTC()) {
				t.Fatalf("v2=%t recovery HTTP fixture must be unpublished independently of evaluation holds", v2)
			}
			store := &privateScoreOwnerStore{fakeStore: &fakeStore{getJob: job, round: &job.Round,
				holdStatus: competitionAutomationHoldStatus{Evaluation: v2}}, owner: "apex"}
			service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
			previousService := defaultService
			defaultService = func() *Service { return service }
			defer func() { defaultService = previousService }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			endpoint, handler := "/competition/score/", GetScoreHandler
			if v2 {
				endpoint, handler = "/competition/v2/score/", CompetitionV2GetScoreHandler
			}
			transport := router.NewRouter(ctx, []*router.Route{router.NewRoute(http.MethodGet, endpoint+"([^/]+)", handler)})
			for _, phase := range []string{"failed", "succeeded"} {
				job.State = phase
				if phase == "succeeded" {
					job.EvalError, job.Score = nil, privateScoreFeedbackFixture()
				}
				storedBefore, err := json.Marshal(job)
				if err != nil {
					t.Fatal(err)
				}
				checks := []struct {
					name, secret  string
					status, calls int
				}{
					{name: "owner", secret: "submit-secret", status: http.StatusOK, calls: 1},
					{name: "anonymous", status: http.StatusUnauthorized},
					{name: "wrong-owner", secret: "synthetic-recovery-other-token", status: http.StatusNotFound, calls: 1},
					{name: "reviewer", secret: "synthetic-recovery-reviewer-token", status: http.StatusUnauthorized},
					{name: "scheduler", secret: "synthetic-recovery-scheduler-token", status: http.StatusUnauthorized},
					{name: "invalid", secret: "synthetic-recovery-invalid-token", status: http.StatusUnauthorized},
				}
				if v2 {
					checks = append(checks, struct {
						name, secret  string
						status, calls int
					}{name: "v2-operator", secret: "operator-secret", status: http.StatusForbidden})
				}
				for _, check := range checks {
					request := httptest.NewRequest(http.MethodGet, endpoint+job.JobId.String(), nil)
					if check.secret != "" {
						request.Header.Set("Authorization", "Bearer "+check.secret)
					}
					response := httptest.NewRecorder()
					callsBefore := store.calls
					transport.ServeHTTP(response, request)
					var wire map[string]json.RawMessage
					if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
						t.Fatal(err)
					}
					if response.Code != check.status || store.calls-callsBefore != check.calls ||
						response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "synthetic-private-") ||
						strings.Contains(response.Body.String(), receipt.RequestSha256) {
						t.Fatalf("v2=%t phase=%s %s HTTP isolation: status=%d calls=%d headers=%v body=%s", v2, phase, check.name, response.Code, store.calls-callsBefore, response.Header(), response.Body.String())
					}
					if check.status != http.StatusOK {
						if wire["reevaluation"] != nil || wire["score"] != nil || wire["eval_error"] != nil ||
							strings.Contains(response.Body.String(), cycle.ReceiptSha256) || strings.Contains(response.Body.String(), cycle.AuthorizationSha256) {
							t.Fatalf("v2=%t %s denial disclosed owned recovery authority", v2, check.name)
						}
						continue
					}
					var result ScoreJobResult
					if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if result.Reevaluation == nil || !reflect.DeepEqual(result.Reevaluation, &receipt.CompetitionJobReevaluationCycle) ||
						model.ValidateCompetitionJobReevaluationCycle(result.Reevaluation) != nil || result.JobId != job.JobId || result.RoundId != job.RoundId ||
						result.State != "completed" || result.EvalError != nil || result.StartedAt == nil || !result.StartedAt.Equal(originalStartedAt) {
						t.Fatalf("v2=%t phase=%s owned serialization lost or changed its canonical recovery: %+v", v2, phase, result)
					}
					var publicCycle map[string]json.RawMessage
					if err := json.Unmarshal(wire["reevaluation"], &publicCycle); err != nil {
						t.Fatal(err)
					}
					for _, key := range []string{"request_sha256", "attempt_offset", "expected_terminal_snapshot", "authorization", "original_terminal_json"} {
						if publicCycle[key] != nil {
							t.Fatalf("owned recovery exposed private audit key %q", key)
						}
					}
					if phase == "failed" {
						if result.EvaluationStatus != "failed" || result.EvaluationFailure == nil || *result.EvaluationFailure != cycle.PreviousFailure || result.Score != nil {
							t.Fatal("recovered failure escaped as a score, full error, or unknown public failure")
						}
					} else if result.EvaluationStatus != "completed" || result.EvaluationFailure != nil || result.Score == nil ||
						!privateScoreProjectionMatches(result.Score) || !reflect.DeepEqual(result.Score, privateCompletedScore(job.Score)) {
						t.Fatal("recovered completed score bypassed the existing private-feedback projection")
					}
					if v2 && (result.DecisionMode != model.CompetitionDecisionModeV2 || result.RequiredApiCapability != settings.AutomationPolicy.RequiredApiCapability) ||
						!v2 && (result.DecisionMode != "" || result.RequiredApiCapability != "") {
						t.Fatal("recovery silently changed its frozen v1/v2 transport contract")
					}
				}
				storedAfter, err := json.Marshal(job)
				if err != nil || !bytes.Equal(storedBefore, storedAfter) {
					t.Fatal("HTTP projection or refusal mutated the stored outcome, marker, or frozen policy")
				}
			}
			if v2 {
				store.holdStatusErr = errors.New("synthetic-private-recovery-hold-storage")
				request := httptest.NewRequest(http.MethodGet, endpoint+job.JobId.String(), nil)
				request.Header.Set("Authorization", "Bearer submit-secret")
				response := httptest.NewRecorder()
				transport.ServeHTTP(response, request)
				var wire map[string]json.RawMessage
				if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
					t.Fatal(err)
				}
				if response.Code != http.StatusServiceUnavailable || response.Header().Get("Cache-Control") != "no-store" ||
					wire["reevaluation"] != nil || wire["score"] != nil || strings.Contains(response.Body.String(), "synthetic-private-") {
					t.Fatal("hold storage failure published recovery authority, private evidence, or reusable output")
				}
			}
			receiptAfter, err := json.Marshal(receipt)
			if err != nil || !bytes.Equal(receiptBefore, receiptAfter) {
				t.Fatal("HTTP polling rewrote the original stored reevaluation receipt")
			}
		}()
	}
}
