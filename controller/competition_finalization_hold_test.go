// Finalization status is prospective. It never asserts missing readiness,
// discloses private evidence or hides a previously authorized published seal.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"gopkg.in/yaml.v3"
)

// Authentication records only the scoped control request, without a database
// or a broad legacy operator fallback. Other Store methods remain the fixture.
type competitionFinalizationControlStore struct {
	*fakeStore
	calls   int
	actor   string
	request model.CompetitionAutomationHoldArgs
}

// The response uses the unchanged append-only public hold shape.
func (self *competitionFinalizationControlStore) SetAutomationHold(_ context.Context, settings *Settings, actor string, request model.CompetitionAutomationHoldArgs) (*model.CompetitionAutomationHold, error) {
	self.calls++
	self.actor, self.request = actor, request
	return &model.CompetitionAutomationHold{HoldId: server.NewId(), Environment: settings.AutomationPolicy.Environment,
		CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId, ActorId: actor, Revision: 1, Request: request}, nil
}

// The optional store interface must not confer incident authority in this test.
func (self *competitionFinalizationControlStore) RecordAutomationIncident(context.Context, *Settings, server.Id, string, model.CompetitionAutomationIncidentArgs) (*model.CompetitionAutomationIncident, error) {
	panic("finalization request unexpectedly became an integrity incident")
}

// The real handler authenticates the precise scheduler and every scope before
// any store call. Neither existing operator nor other control roles overlap.
func TestCompetitionFinalizationHoldAuthenticatedHandlerScope(t *testing.T) {
	settings := automationApiSettings()
	store := &competitionFinalizationControlStore{fakeStore: &fakeStore{}}
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	previousService := defaultService
	defaultService = func() *Service { return service }
	defer func() { defaultService = previousService }()
	args := competitionFinalizationHoldArgs("synthetic-http-finalization")
	content, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		role         string
		foreignScope bool
		wantStatus   int
	}{
		{role: competitionSchedulerRole, wantStatus: http.StatusOK},
		{role: competitionSchedulerRole, foreignScope: true, wantStatus: http.StatusForbidden},
		{role: competitionSourcePromoterRole, wantStatus: http.StatusForbidden},
		{role: competitionSettlementRole, wantStatus: http.StatusForbidden},
		{role: competitionReviewerRole, wantStatus: http.StatusForbidden},
		{role: "operator", wantStatus: http.StatusForbidden},
		{role: "submitter", wantStatus: http.StatusForbidden},
	} {
		secret := "synthetic-finalization-token-" + test.role
		digest := sha256.Sum256([]byte(secret))
		token := Token{Name: "synthetic-finalization-actor", Role: test.role, Sha256: hex.EncodeToString(digest[:]),
			Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId}
		if test.foreignScope {
			token.SeasonId = "synthetic-other-season"
		}
		settings.Tokens = []Token{token}
		request := httptest.NewRequest(http.MethodPost, "/competition/v2/holds", bytes.NewReader(content))
		request.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		before := store.calls
		CompetitionV2HoldHandler(response, request)
		if response.Code != test.wantStatus || (store.calls == before+1) != (test.wantStatus == http.StatusOK) {
			t.Fatalf("role=%s foreign=%t status=%d calls=%d", test.role, test.foreignScope, response.Code, store.calls-before)
		}
		if test.wantStatus == http.StatusOK && (store.actor != token.Name || store.request != args || response.Header().Get("Cache-Control") != "no-store") {
			t.Fatal("handler changed scoped finalization authority", store.actor, store.request)
		}
	}
}

// The separately versioned public schema keeps the original status fields
// required and adds the new status bit only as an optional prospective signal.
func TestCompetitionFinalizationHoldOpenApiContract(t *testing.T) {
	content, err := os.ReadFile("../../sn/api/competition.yml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Schemas map[string]struct {
				Required   []string `yaml:"required"`
				Properties map[string]struct {
					Type string   `yaml:"type"`
					Enum []string `yaml:"enum"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	got := document.Components.Schemas["CompetitionAutomationHoldArgs"].Properties["kind"].Enum
	want := []string{"admission", "evaluation", "source_activation", "reward_delivery", "finalization"}
	slices.Sort(got)
	slices.Sort(want)
	status := document.Components.Schemas["CompetitionRuntimeStatus"]
	if !reflect.DeepEqual(got, want) || status.Properties["finalization_held"].Type != "boolean" || slices.Contains(status.Required, "finalization_held") || len(status.Required) != 3 {
		t.Fatal("finalization API contract drift", got, status.Required)
	}
}

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
