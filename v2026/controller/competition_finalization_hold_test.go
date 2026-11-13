// Finalization status is prospective. It never asserts missing readiness,
// discloses private evidence or hides a previously authorized published seal.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
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
	content, err := readCompetitionTestSnContract()
	if err != nil {
		t.Fatal(err)
	}
	if err := competitionFinalizationOpenApiContract(content); err != nil {
		t.Fatal(err)
	}
}

// OpenAPI 3.1 permits sequence-valued types elsewhere in the document. Retain
// the syntax tree and decode only the three paths this contract actually owns.
func competitionFinalizationOpenApiContract(content []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return err
	}
	kind, err := competitionFinalizationOpenApiNode(&document, "components", "schemas", "CompetitionAutomationHoldArgs", "properties", "kind", "enum")
	if err != nil {
		return err
	}
	var got []string
	if kind.Kind != yaml.SequenceNode || kind.Decode(&got) != nil {
		return fmt.Errorf("finalization hold kind must retain its explicit enum")
	}
	want := []string{"admission", "evaluation", "source_activation", "reward_delivery", "finalization"}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("finalization hold capability enum changed")
	}
	statusType, err := competitionFinalizationOpenApiNode(&document, "components", "schemas", "CompetitionRuntimeStatus", "properties", "finalization_held", "type")
	if err != nil {
		return err
	}
	if statusType.Kind != yaml.ScalarNode || statusType.Tag != "!!str" || statusType.Value != "boolean" {
		return fmt.Errorf("finalization runtime status must be an explicit boolean")
	}
	required, err := competitionFinalizationOpenApiNode(&document, "components", "schemas", "CompetitionRuntimeStatus", "required")
	if err != nil {
		return err
	}
	var requiredFields []string
	if required.Kind != yaml.SequenceNode || required.Decode(&requiredFields) != nil {
		return fmt.Errorf("runtime status must retain its required-field contract")
	}
	slices.Sort(requiredFields)
	if !reflect.DeepEqual(requiredFields, []string{"grading_ready", "intake_ready", "publication_held"}) {
		return fmt.Errorf("finalization status must remain optional without changing legacy required fields")
	}
	return nil
}

// Missing or incorrectly shaped targeted paths fail closed, without projecting
// unrelated schema properties into a narrower Go representation of OpenAPI.
func competitionFinalizationOpenApiNode(node *yaml.Node, path ...string) (*yaml.Node, error) {
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	for index, key := range path {
		if node.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("OpenAPI contract path %s is not an object", strings.Join(path[:index], "."))
		}
		var next *yaml.Node
		for entry := 0; entry+1 < len(node.Content); entry += 2 {
			if node.Content[entry].Kind == yaml.ScalarNode && node.Content[entry].Value == key {
				next = node.Content[entry+1]
				break
			}
		}
		if next == nil {
			return nil, fmt.Errorf("OpenAPI contract path %s is missing", strings.Join(path[:index+1], "."))
		}
		node = next
	}
	return node, nil
}

// An unrelated valid nullable union reproduces the original parser failure;
// ignoring that unrelated shape must not weaken any owned-field assertion.
func TestCompetitionFinalizationHoldOpenApiTargetedProjection(t *testing.T) {
	const document = `openapi: 3.1.0
components:
  schemas:
    Unrelated:
      properties:
        nullable: {type: [string, null]}
    CompetitionAutomationHoldArgs:
      properties:
        kind: {enum: [admission, evaluation, source_activation, reward_delivery, finalization]}
    CompetitionRuntimeStatus:
      required: [intake_ready, grading_ready, publication_held]
      properties:
        finalization_held: {type: boolean}
`
	if err := competitionFinalizationOpenApiContract([]byte(document)); err != nil {
		t.Fatal("valid unrelated OpenAPI union changed finalization contract", err)
	}
	for _, test := range []struct{ name, old, replacement string }{
		{"missing kind enum", "kind: {enum:", "other: {enum:"},
		{"missing capability", "reward_delivery, finalization", "reward_delivery"},
		{"unknown capability", "reward_delivery, finalization", "reward_delivery, finalization, unrelated"},
		{"missing runtime property", "finalization_held: {type: boolean}", "other: {type: boolean}"},
		{"wrong runtime type", "finalization_held: {type: boolean}", "finalization_held: {type: string}"},
		{"nullable target type", "finalization_held: {type: boolean}", "finalization_held: {type: [boolean, null]}"},
		{"required finalization status", "grading_ready, publication_held]", "grading_ready, publication_held, finalization_held]"},
		{"missing legacy required field", "grading_ready, publication_held]", "grading_ready]"},
	} {
		changed := strings.Replace(document, test.old, test.replacement, 1)
		if changed == document || competitionFinalizationOpenApiContract([]byte(changed)) == nil {
			t.Fatalf("targeted contract accepted %s", test.name)
		}
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
