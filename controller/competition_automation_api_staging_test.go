// Staging exercises the same authenticated source and containment boundaries
// as production without using a production round or payment side effect.
package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type automationStagingApiStore struct {
	*automationReadinessStore
	scopedCalls int
	readyCalls  int
}

// A staging season cannot substitute the unrelated production row.
func (self *automationStagingApiStore) CurrentAutomationRound(context.Context, *Settings) (*roundRecord, error) {
	self.scopedCalls++
	return self.stagingRound, nil
}

// Source and containment are independent admission requirements.
func (self *automationStagingApiStore) Readiness(ctx context.Context, settings *Settings) (map[string]bool, error) {
	self.readyCalls++
	checks, err := self.automationReadinessStore.Readiness(ctx, settings)
	for name, passed := range self.fakeStore.readiness {
		checks[name] = passed
	}
	checks["staging_mode"] = settings.AutomationPolicy.Staging && settings.EvaluationPolicy.TakeoverMargin == 0 && settings.SeasonPolicy.SubmissionFeeUsd == 0
	return checks, err
}

// Uses only synthetic scope, exact fake store authority, and a private archive
// fixture; it does not stand in for real host qualification evidence.
func newAutomationStagingApiFixture(t testing.TB) (*Settings, *automationStagingApiStore, *Service) {
	t.Helper()
	settings := automationApiSettings()
	settings.AutomationPolicy.Staging = true
	settings.AutomationPolicy.StagingRoundWindowSeconds = 600
	settings.AutomationPolicy.StagingPreparationSeconds = 0
	settings.SeasonPolicy.SubmissionWindowSeconds = 600
	settings.SeasonPolicy.PreparationWindowSeconds = 0
	settings.SeasonPolicy.SubmissionFeeUsd = 0
	settings.EvaluationPolicy.TakeoverMargin = 0
	settings.EvaluationPolicy.Replicates = 9
	if err := settings.Validate(); err != nil {
		t.Fatalf("invalid staging API fixture: %v", err)
	}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	intent := &model.CompetitionSourceActivation{
		Staging: true, StagingRoundWindowSeconds: 600, StagingPreparationSeconds: 0,
		Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
		Generation: 2, SourceGeneration: 0, TargetEpoch: 2, TargetRoundId: server.NewId(),
		EvaluatorImageDigest: settings.EvaluatorImageDigest, HostQualificationSha256: strings.Repeat("9", 64), Threshold: 0,
		ActivatedAt: &now, State: "activated", Commits: map[string]string{},
	}
	for _, name := range []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "operator-proxy", "warp", "gvisor"} {
		intent.Commits[name] = strings.Repeat("c", 40)
	}
	var err error
	intent.SourceLockSha256, err = model.CompetitionSourceLockDigest(intent.Commits)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := sourceSettingsFromIntent(settings, intent)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := policySnapshot(frozen)
	if err != nil {
		t.Fatal(err)
	}
	round := &roundRecord{RoundResult: RoundResult{RoundId: intent.TargetRoundId, Epoch: intent.TargetEpoch, Staging: true, Status: "open"}, PolicyJson: policy}
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: round.RoundId, Staging: true, State: "queued"}, Round: *round}
	store := &automationStagingApiStore{automationReadinessStore: &automationReadinessStore{fakeStore: &fakeStore{stagingRound: round, enqueueJob: job}, intent: intent}}
	return settings, store, newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
}

// Readiness loads the scoped staging round and its genuinely activated source.
func TestCompetitionV2StagingReadinessUsesScopedActivatedSource(t *testing.T) {
	settings, store, service := newAutomationStagingApiFixture(t)
	result, evalError := service.Ready(context.Background())
	if evalError != nil || !result.Ready || store.scopedCalls != 1 || store.readyCalls != 1 {
		t.Fatalf("staging source was not selected: %+v %+v scoped=%d ready=%d", result, evalError, store.scopedCalls, store.readyCalls)
	}
	if settings.BaseSha == store.intent.Commits["server"] || settings.AutomationPolicy.SourceActivationGeneration != 1 {
		t.Fatal("readiness rewrote season configuration")
	}
	store.intent.ActivatedAt = nil
	result, evalError = service.Ready(context.Background())
	if evalError == nil || evalError.Code != "source_not_ready" || result.Ready || store.readyCalls != 1 {
		t.Fatalf("unactivated staging source became ready: %+v %+v", result, evalError)
	}
}

// A mixed-version store fails rather than falling back to a production row.
func TestCompetitionV2StagingReadinessRequiresScopedStore(t *testing.T) {
	settings, store, _ := newAutomationStagingApiFixture(t)
	if _, err := competitionConfiguredCurrentRound(context.Background(), settings, store.fakeStore); !errors.Is(err, ErrUnsupportedDecisionMode) {
		t.Fatalf("old store silently handled staging: %v", err)
	}
}

// Previously the staging branch skipped source activation and enqueued anyway.
func TestCompetitionV2StagingSubmissionRejectsUnactivatedSource(t *testing.T) {
	_, store, service := newAutomationStagingApiFixture(t)
	store.intent.ActivatedAt = nil
	args := ScoreArgs{RoundId: store.stagingRound.RoundId, Patch: testPatch("synthetic-unactivated")}
	_, status, evalError := service.SubmitV2(context.Background(), args, &Principal{Id: "synthetic-submitter", Role: "submitter"})
	if status != http.StatusServiceUnavailable || evalError == nil || evalError.Code != "round_source_not_ready" || store.enqueueCalls != 0 || store.readyCalls != 0 {
		t.Fatalf("staging bypassed activation: status=%d error=%+v enqueue=%d ready=%d", status, evalError, store.enqueueCalls, store.readyCalls)
	}
}

// A staging marker is not permission to skip the evaluator containment gate.
func TestCompetitionV2StagingSubmissionRequiresContainment(t *testing.T) {
	_, store, service := newAutomationStagingApiFixture(t)
	store.fakeStore.readiness = map[string]bool{"containment": false}
	args := ScoreArgs{RoundId: store.stagingRound.RoundId, Patch: testPatch("synthetic-containment")}
	_, status, evalError := service.SubmitV2(context.Background(), args, &Principal{Id: "synthetic-submitter", Role: "submitter"})
	if status != http.StatusServiceUnavailable || evalError == nil || evalError.Code != "not_ready" || store.enqueueCalls != 0 || store.readyCalls != 1 {
		t.Fatalf("staging bypassed containment: status=%d error=%+v enqueue=%d ready=%d", status, evalError, store.enqueueCalls, store.readyCalls)
	}
	store.fakeStore.readiness["containment"] = true
	accepted, status, evalError := service.SubmitV2(context.Background(), args, &Principal{Id: "synthetic-submitter", Role: "submitter"})
	if status != http.StatusAccepted || evalError != nil || accepted == nil || !accepted.Staging || store.enqueueCalls != 1 {
		t.Fatalf("qualified staging admission failed: %+v status=%d error=%+v", accepted, status, evalError)
	}
}

// The row marker and frozen policy must agree before polling or fee screening.
func TestCompetitionV2StagingContractRejectsCrossModeRow(t *testing.T) {
	_, store, _ := newAutomationStagingApiFixture(t)
	for _, staging := range []bool{false, true} {
		round := *store.stagingRound
		if staging {
			// A production policy remains intrinsically valid on its own.
			production, err := policySnapshot(automationApiSettings())
			if err != nil {
				t.Fatal(err)
			}
			round.PolicyJson = production
		}
		round.Staging = staging
		status, evalError := requireRoundDecisionContract(&round, true)
		if status != http.StatusServiceUnavailable || evalError == nil || evalError.Code != "round_policy_mismatch" {
			t.Fatalf("cross-mode row passed: staging=%t status=%d error=%+v", staging, status, evalError)
		}
	}
}
