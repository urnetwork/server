// Synthetic authorities and retained-envelope fakes exercise intake admission
// without live databases, host probes, credentials or production identities.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The SQL seam returns current column facts separately from model JSON.
type competitionIntakeReadinessReader struct {
	stateLock         sync.Mutex
	records           map[string]map[string]any
	err               error
	reads             int
	source            *model.CompetitionSourceActivation
	sourceGeneration  int64
	sourceInvalidated bool
	sourceErr         error
	sourceReads       int
}

// No query can acquire a real connection or use a production fixture.
func (self *competitionIntakeReadinessReader) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	return competitionIntakeReadinessRow(func(destinations ...any) error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if strings.Contains(query, "FROM competition_source_activation") {
			self.sourceReads++
			if self.sourceErr != nil {
				return self.sourceErr
			}
			if self.source == nil {
				return pgx.ErrNoRows
			}
			content, err := json.Marshal(self.source)
			if err != nil {
				return err
			}
			*destinations[0].(*[]byte) = content
			*destinations[1].(*string) = self.source.IntentSha256
			*destinations[2].(**time.Time) = self.source.ActivatedAt
			*destinations[3].(**time.Time) = self.source.AbandonedAt
			*destinations[4].(*int64) = self.sourceGeneration
			*destinations[5].(*bool) = self.sourceInvalidated
			return nil
		}
		self.reads++
		if self.err != nil {
			return self.err
		}
		content, err := json.Marshal(self.records)
		if err == nil {
			*destinations[0].(*[]byte) = content
		}
		return err
	})
}

// Scan is synchronous, like the caller-owned transaction's row.
type competitionIntakeReadinessRow func(...any) error

// The fake populates only the aggregate's single typed destination.
func (self competitionIntakeReadinessRow) Scan(destinations ...any) error {
	return self(destinations...)
}

// API fakes retain actual source-envelope authentication and raw host failures.
type competitionIntakeReadinessStore struct {
	*competitionAutomationStagingPreflightFixture
	reader            *competitionIntakeReadinessReader
	archiveStart      chan struct{}
	archiveReturn     chan struct{}
	marked            bool
	clock             func() time.Time
	beforeAdmission   func()
	maintenanceMarked bool
}

// All non-host dependencies remain present; tests may independently fail each.
func (self *competitionIntakeReadinessStore) Readiness(ctx context.Context, settings *Settings) (map[string]bool, error) {
	checks, err := self.automationReadinessStore.Readiness(ctx, settings)
	for _, name := range []string{"configuration", "frozen_policy", "retention_window", "database", "fifo_slot", "queue_admission"} {
		checks[name] = true
	}
	for _, name := range []string{"authoritative_evaluator_host", "artifact_storage", "host_rebaseline"} {
		checks[name] = false
	}
	for name, passed := range self.fakeStore.readiness {
		checks[name] = passed
	}
	return checks, err
}

// This is the same typed current-row reader as the durable guard.
func (self *competitionIntakeReadinessStore) AutomationHoldStatus(ctx context.Context, settings *Settings) (competitionAutomationHoldStatus, error) {
	return readCompetitionIntakeHolds(ctx, self.reader, settings)
}

// The synthetic two-pass owner exercises the real permit guard around an
// explicit retained-upload barrier, without claiming PostgreSQL race coverage.
func (self *competitionIntakeReadinessStore) Enqueue(ctx context.Context, settings *Settings, roundId server.Id, patch *CanonicalPatch, principal, image string) (*queuedJob, bool, error) {
	self.marked = settings.intakeAdmission != nil
	self.maintenanceMarked = self.marked && settings.intakeAdmission.MaintenanceAuthority != nil
	receivedAt := server.NowUtc()
	if self.beforeAdmission != nil {
		self.beforeAdmission()
	}
	checkAdmission := func() error {
		if self.clock != nil {
			return checkCompetitionIntakeAdmissionWithClock(ctx, self.reader, settings, self.round, receivedAt, self.clock)
		}
		return checkCompetitionIntakeAdmission(ctx, self.reader, settings, self.round, receivedAt)
	}
	if err := checkAdmission(); err != nil {
		return nil, false, err
	}
	if self.archiveStart != nil {
		close(self.archiveStart)
		<-self.archiveReturn
	}
	if err := checkAdmission(); err != nil {
		return nil, false, err
	}
	return self.fakeStore.Enqueue(ctx, settings, roundId, patch, principal, image)
}

// Builds a production-shaped but wholly synthetic initially activated source;
// its blob envelope is published through the actual retained fake adapter.
func newCompetitionIntakeReadinessFixture(t testing.TB) (*Settings, *competitionIntakeReadinessStore, *Service, *competitionRuntimeBlobTestStore) {
	t.Helper()
	settings, base, archive, blobs := newCompetitionRuntimeBlobFixture(t)
	settings.AutomationPolicy.Staging = false
	settings.AutomationPolicy.StagingRoundWindowSeconds, settings.AutomationPolicy.StagingPreparationSeconds = 0, 0
	settings.SeasonPolicy.SubmissionWindowSeconds, settings.SeasonPolicy.PreparationWindowSeconds, settings.SeasonPolicy.SubmissionFeeUsd = 604800, 0, 20
	settings.EvaluationPolicy.TakeoverMargin = .269
	intent := base.intent
	intent.Staging, intent.StagingRoundWindowSeconds, intent.StagingPreparationSeconds = false, 0, 0
	intent.Generation, intent.TargetEpoch, intent.Threshold = 1, 1, settings.EvaluationPolicy.TakeoverMargin
	intent.Manifest = []byte("schema: 2\nsynthetic: initial-intake-envelope\n")
	digest := sha256.Sum256(intent.Manifest)
	intent.ManifestSha256 = hex.EncodeToString(digest[:])
	var err error
	intent.IntentSha256, err = competitionPreparationIntentDigest(*intent)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := sourceSettingsFromIntent(settings, intent)
	if err != nil {
		t.Fatal(err)
	}
	round := base.stagingRound
	round.Staging, round.Epoch, round.CompetitionId = false, 1, settings.CompetitionId
	round.OpensAt, round.ClosesAt = server.NowUtc().Add(-time.Hour), server.NowUtc().Add(time.Hour)
	round.PolicyJson, err = policySnapshot(frozen)
	if err != nil {
		t.Fatal(err)
	}
	base.round = round
	base.enqueueJob.Round, base.enqueueJob.Staging = *round, false
	if _, err := archive.publishRuntimeManifest(context.Background(), settings, intent); err != nil {
		t.Fatal(err)
	}
	ownerSha256 := strings.Repeat("d", 64)
	reader := &competitionIntakeReadinessReader{records: map[string]map[string]any{}}
	for index, kind := range []string{"evaluation", "reward_delivery"} {
		args := model.CompetitionAutomationHoldArgs{Kind: kind, Active: true, ExpectedRevision: 0, IdempotencyKey: "synthetic-" + kind,
			ReasonCode: "OPERATOR_HOLD", PublicExplanation: "Synthetic deferred gate.", EvidenceSha256: ownerSha256}
		requestSha256, err := model.CompetitionAutomationDigest(args)
		if err != nil {
			t.Fatal(err)
		}
		holdId := server.Id{byte(index + 1)}
		hold := &model.CompetitionAutomationHold{HoldId: holdId, Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId,
			SeasonId: settings.AutomationPolicy.SeasonId, ActorId: "synthetic-owner", Revision: 1, CreatedAt: server.NowUtc(), Request: args, RequestSha256: requestSha256}
		reader.records[kind] = competitionIntakeReadinessHoldRecord(hold)
	}
	store := &competitionIntakeReadinessStore{competitionAutomationStagingPreflightFixture: base, reader: reader}
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	evaluation := reader.records["evaluation"]["hold"].(*model.CompetitionAutomationHold)
	reward := reader.records["reward_delivery"]["hold"].(*model.CompetitionAutomationHold)
	service.intakeAuthority = competitionIntakeAuthority{Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
		RoundId: round.RoundId.String(), PolicySha256: settings.AutomationPolicy.PolicyApprovalSha256, OwnerSha256: ownerSha256,
		BaseSha: frozen.BaseSha, ImageDigest: frozen.EvaluatorImageDigest, SourceLockSha256: frozen.AutomationPolicy.SourceLockSha256,
		HardwareId: frozen.EvaluationPolicy.HardwareId, HostQualificationSha256: frozen.EvaluationPolicy.HostQualificationSha256,
		EvaluationHoldId: evaluation.HoldId.String(), EvaluationRequestSha256: evaluation.RequestSha256, RewardHoldId: reward.HoldId.String(), RewardRequestSha256: reward.RequestSha256}
	return settings, store, service, blobs
}

// Synthetic column facts remain independently mutable from the JSON payload.
func competitionIntakeReadinessHoldRecord(hold *model.CompetitionAutomationHold) map[string]any {
	return map[string]any{"active": hold.Request.Active, "revision": hold.Revision, "hold_id": hold.HoldId.String(),
		"created_at": hold.CreatedAt.Truncate(time.Microsecond), "idempotency_key": hold.Request.IdempotencyKey,
		"request_sha256": hold.RequestSha256, "hold": hold}
}

// The captured root cause is host-only unavailability during approved held
// intake: real API entry points still authenticate the retained source first.
func TestCompetitionApprovedIntakeQueuesWithoutOnlineHost(t *testing.T) {
	settings, store, service, _ := newCompetitionIntakeReadinessFixture(t)
	ready, readyErr := service.Ready(context.Background())
	if readyErr == nil || ready.Ready || ready.Checks["authoritative_evaluator_host"] || ready.RuntimeStatus == nil ||
		!ready.RuntimeStatus.IntakeReady || ready.RuntimeStatus.GradingReady || !ready.RuntimeStatus.PublicationHeld {
		t.Fatalf("offline worker became grading-ready or blocked approved intake: %+v %v", ready, readyErr)
	}
	info, infoErr := service.Info(context.Background())
	if infoErr != nil || info.RuntimeStatus == nil || !info.RuntimeStatus.IntakeReady || info.RuntimeStatus.GradingReady || !info.RuntimeStatus.PublicationHeld {
		t.Fatalf("approved intake discovery failed: %+v %v", info, infoErr)
	}
	accepted, status, submitErr := service.SubmitV2(context.Background(), ScoreArgs{RoundId: store.round.RoundId, Patch: testPatch("synthetic-offline-intake")}, &Principal{Id: "synthetic-submitter", Role: "submitter"})
	if submitErr != nil || status != http.StatusAccepted || accepted.State != "queued" || store.enqueueCalls != 1 || !store.marked || settings.intakeAdmission != nil {
		t.Fatalf("approved intake did not queue with an isolated permit: %+v %d %v", accepted, status, submitErr)
	}
}

// Initial source authentication, private artifacts, identity and authorization
// remain independent hard gates even when all three host flags are false.
func TestCompetitionApprovedIntakePreservesSourceArtifactsAndAuth(t *testing.T) {
	_, store, service, blobs := newCompetitionIntakeReadinessFixture(t)
	args := ScoreArgs{RoundId: store.round.RoundId, Patch: testPatch("synthetic-guard")}
	principal := &Principal{Id: "synthetic-submitter", Role: "submitter"}
	activatedAt := store.intent.ActivatedAt
	store.intent.ActivatedAt = nil
	if _, status, err := service.SubmitV2(context.Background(), args, principal); status != http.StatusServiceUnavailable || err == nil || store.enqueueCalls != 0 {
		t.Fatal("unactivated source acquired intake permission")
	}
	store.intent.ActivatedAt = activatedAt
	blobs.protectionErr = errors.New("synthetic missing protection")
	if _, status, err := service.SubmitV2(context.Background(), args, principal); status != http.StatusServiceUnavailable || err == nil || store.enqueueCalls != 0 {
		t.Fatal("missing retained protection acquired intake permission")
	}
	blobs.protectionErr = nil
	if _, status, err := service.SubmitV2(context.Background(), args, &Principal{Id: "synthetic-operator", Role: "operator"}); status != http.StatusForbidden || err == nil || store.enqueueCalls != 0 {
		t.Fatal("non-submitter acquired intake permission")
	}
	service.apiImageIdentityErr = errors.New("synthetic image identity outage")
	if _, status, err := service.SubmitV2(context.Background(), args, principal); status != http.StatusServiceUnavailable || err == nil || store.enqueueCalls != 0 {
		t.Fatal("unknown API image acquired intake permission")
	}
}

// No ordinary paused scope, later generation, altered approval or expired
// window can borrow this content-addressed initial authority.
func TestCompetitionApprovedIntakeSelectorRefusesAdjacentScopes(t *testing.T) {
	settings, store, service, _ := newCompetitionIntakeReadinessFixture(t)
	frozen, err := settingsForAutomationRound(context.Background(), settings, store, store.round)
	if err != nil {
		t.Fatal(err)
	}
	now := server.NowUtc()
	for _, change := range []func(*Settings, *roundRecord){
		func(s *Settings, _ *roundRecord) { s.AutomationPolicy.PolicyApprovalSha256 = strings.Repeat("e", 64) },
		func(s *Settings, _ *roundRecord) { s.AutomationPolicy.Environment = "synthetic-other-environment" },
		func(s *Settings, _ *roundRecord) { s.AutomationPolicy.SeasonId = "synthetic-other-season" },
		func(s *Settings, _ *roundRecord) { s.AutomationPolicy.Staging = true },
		func(s *Settings, _ *roundRecord) { s.AutomationPolicy.SourceGeneration = 1 },
		func(s *Settings, _ *roundRecord) { s.AutomationPolicy.SourceActivationGeneration = 2 },
		func(s *Settings, _ *roundRecord) { s.BaseSha = strings.Repeat("f", 40) },
		func(_ *Settings, r *roundRecord) { r.Epoch = 2 },
		func(_ *Settings, r *roundRecord) { r.Canceled = true },
		func(_ *Settings, r *roundRecord) { r.ClosesAt = now },
		func(_ *Settings, r *roundRecord) { r.OpensAt = now.Add(time.Hour) },
	} {
		candidate, round := *frozen, *store.round
		change(&candidate, &round)
		if service.intakeAuthority.matches(&candidate, &round, now) {
			t.Fatal("adjacent source/scope/window acquired initial authority")
		}
	}
}

// The exception admits only the three specific host negatives; it cannot
// convert a partial map or a new infrastructure failure into an intake pass.
func TestCompetitionApprovedIntakeChecksRefuseMissingAndUnknown(t *testing.T) {
	settings, store, _, _ := newCompetitionIntakeReadinessFixture(t)
	frozen, err := settingsForAutomationRound(context.Background(), settings, store, store.round)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := store.Readiness(context.Background(), frozen)
	if err != nil {
		t.Fatal(err)
	}
	checks["api_image_identity"], checks["artifact_archive"] = true, true
	if !competitionIntakeChecksPass(checks) {
		t.Fatal("host-only negatives were not isolated")
	}
	for _, name := range []string{"configuration", "frozen_policy", "retention_window", "database", "fifo_slot", "queue_admission", "api_image_identity", "artifact_archive"} {
		delete(checks, name)
		if competitionIntakeChecksPass(checks) {
			t.Fatalf("missing %s was silently passed", name)
		}
		checks[name] = true
	}
	checks["synthetic-new-negative"] = false
	if competitionIntakeChecksPass(checks) {
		t.Fatal("unknown negative was silently passed")
	}
}

// Both exact initial hold identities and column revisions matter; ordinary
// incident holds and release/rehold cannot re-enable the compatibility path.
func TestCompetitionApprovedIntakeHoldsRefuseReleaseReholdAndDrift(t *testing.T) {
	settings, store, service, _ := newCompetitionIntakeReadinessFixture(t)
	status, err := store.AutomationHoldStatus(context.Background(), settings)
	if err != nil || !service.intakeAuthority.holdsMatch(status) {
		t.Fatal("synthetic initial holds did not match", err)
	}
	for _, change := range []func(*competitionAutomationHoldStatus){
		func(s *competitionAutomationHoldStatus) { s.Admission = true },
		func(s *competitionAutomationHoldStatus) { s.Evaluation = false },
		func(s *competitionAutomationHoldStatus) { s.RewardDelivery = false },
		func(s *competitionAutomationHoldStatus) { s.RewardDeliveryHold = nil },
		func(s *competitionAutomationHoldStatus) { s.EvaluationHold.Revision = 3 },
		func(s *competitionAutomationHoldStatus) { s.EvaluationHold.Request.Kind = "admission" },
		func(s *competitionAutomationHoldStatus) { s.EvaluationHold.Request.ReasonCode = "INTEGRITY_INCIDENT" },
		func(s *competitionAutomationHoldStatus) {
			s.EvaluationHold.Request.EvidenceSha256 = strings.Repeat("e", 64)
		},
		func(s *competitionAutomationHoldStatus) { s.EvaluationHold.RequestSha256 = strings.Repeat("e", 64) },
	} {
		candidate := status
		evaluation, reward := *status.EvaluationHold, *status.RewardDeliveryHold
		candidate.EvaluationHold, candidate.RewardDeliveryHold = &evaluation, &reward
		change(&candidate)
		if service.intakeAuthority.holdsMatch(candidate) {
			t.Fatal("changed initial hold acquired authority")
		}
	}
}

// Malformed JSON cannot clear an active column-backed hold for ordinary
// publication consumers, and an unavailable latest read never means unheld.
func TestCompetitionApprovedIntakeHoldReadPreservesColumnAuthority(t *testing.T) {
	settings, store, _, _ := newCompetitionIntakeReadinessFixture(t)
	evaluation := store.reader.records["evaluation"]["hold"].(*model.CompetitionAutomationHold)
	evaluation.Request.Active = false
	if _, err := store.AutomationHoldStatus(context.Background(), settings); err == nil {
		t.Fatal("JSON false silently cleared the active durable column")
	}
	content, err := json.Marshal(evaluation)
	if err != nil {
		t.Fatal(err)
	}
	var malformed map[string]any
	if err := json.Unmarshal(content, &malformed); err != nil {
		t.Fatal(err)
	}
	delete(malformed["request"].(map[string]any), "active")
	store.reader.records["evaluation"]["active"] = false
	store.reader.records["evaluation"]["hold"] = malformed
	if _, err := store.AutomationHoldStatus(context.Background(), settings); err == nil {
		t.Fatal("missing JSON active was treated as an authorized release")
	}
	store.reader.err = errors.New("synthetic latest hold read failure")
	if _, err := store.AutomationHoldStatus(context.Background(), settings); err == nil {
		t.Fatal("hold read failure became unheld")
	}
}

// An explicit barrier forces hold release between admission passes. The real
// guard sees the latest revision, refuses the final mutation and joins owner.
func TestCompetitionApprovedIntakeReleaseDuringArchiveRefusesQueue(t *testing.T) {
	_, store, service, _ := newCompetitionIntakeReadinessFixture(t)
	store.archiveStart, store.archiveReturn = make(chan struct{}), make(chan struct{})
	done := make(chan int, 1)
	go func() {
		_, status, _ := service.SubmitV2(context.Background(), ScoreArgs{RoundId: store.round.RoundId, Patch: testPatch("synthetic-release-race")}, &Principal{Id: "synthetic-submitter", Role: "submitter"})
		done <- status
	}()
	select {
	case <-store.archiveStart:
	case status := <-done:
		t.Fatalf("submission failed before the archive barrier: %d", status)
	case <-t.Context().Done():
		t.Fatal("archive barrier owner canceled")
	}
	store.reader.stateLock.Lock()
	hold := store.reader.records["evaluation"]["hold"].(*model.CompetitionAutomationHold)
	hold.Revision, hold.Request.Active = 2, false
	store.reader.records["evaluation"]["revision"], store.reader.records["evaluation"]["active"] = 2, false
	store.reader.stateLock.Unlock()
	close(store.archiveReturn)
	if status := <-done; status != http.StatusServiceUnavailable || store.enqueueCalls != 0 {
		t.Fatalf("released hold raced into a queued mutation: status=%d calls=%d", status, store.enqueueCalls)
	}
}

// An on-time receipt retains its original timestamp on the post-upload pass;
// expiry of wall time alone must not alter the established submission policy.
func TestCompetitionApprovedIntakeGuardUsesReceiptTime(t *testing.T) {
	settings, store, service, _ := newCompetitionIntakeReadinessFixture(t)
	frozen, err := settingsForAutomationRound(context.Background(), settings, store, store.round)
	if err != nil {
		t.Fatal(err)
	}
	frozen.intakeAdmission = &competitionIntakeAdmission{Authority: service.intakeAuthority}
	// Force the current clock beyond close before either pass. An old guard
	// that rereads wall time now fails without any sleep or scheduler race.
	store.round.OpensAt, store.round.ClosesAt = server.NowUtc().Add(-3*time.Hour), server.NowUtc().Add(-2*time.Hour)
	receivedAt := store.round.ClosesAt.Add(-time.Nanosecond)
	if err := checkCompetitionIntakeAdmission(context.Background(), store.reader, frozen, store.round, receivedAt); err != nil {
		t.Fatal("on-time receipt was rejected before upload", err)
	}
	if err := checkCompetitionIntakeAdmission(context.Background(), store.reader, frozen, store.round, receivedAt); err != nil {
		t.Fatal("on-time receipt was reinterpreted after upload", err)
	}
	if err := checkCompetitionIntakeAdmission(context.Background(), store.reader, frozen, store.round, store.round.ClosesAt); err == nil {
		t.Fatal("late receipt was admitted")
	}
}
