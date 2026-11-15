// Entirely synthetic authorities exercise bounded maintenance intake through
// real API admission and both durable guards, with explicit upload barriers.
package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The new evaluation approval deliberately cannot match the initial permit.
// No production identities, main configuration, or live resources are loaded.
func newCompetitionCalibrationMaintenanceFixture(t testing.TB) (*Settings, *competitionIntakeReadinessStore, *Service, *competitionRuntimeBlobTestStore) {
	t.Helper()
	settings, store, service, blobs := newCompetitionIntakeReadinessFixture(t)
	args := model.CompetitionAutomationHoldArgs{
		Kind: "evaluation", Active: true, ExpectedRevision: 3,
		IdempotencyKey: "synthetic-calibration-maintenance-once", ReasonCode: "CALIBRATION_MAINTENANCE",
		PublicExplanation: "Synthetic bounded serial calibration with authenticated intake preserved.", EvidenceSha256: strings.Repeat("9", 64),
	}
	digest, err := model.CompetitionAutomationDigest(args)
	if err != nil {
		t.Fatal(err)
	}
	hold := &model.CompetitionAutomationHold{
		HoldId: server.Id{9}, Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId,
		SeasonId: settings.AutomationPolicy.SeasonId, ActorId: "synthetic-calibration-owner", Revision: 4,
		CreatedAt: server.NowUtc().Truncate(time.Microsecond), Request: args, RequestSha256: digest,
	}
	store.reader.records["evaluation"] = competitionIntakeReadinessHoldRecord(hold)
	store.reader.source, store.reader.sourceGeneration = store.intent, store.intent.Generation
	service.calibrationMaintenanceAuthority = competitionCalibrationMaintenanceAuthority{
		Scope: service.intakeAuthority, OwnerSha256: args.EvidenceSha256, EvaluationRevision: hold.Revision,
		EvaluationIdempotencyKey: args.IdempotencyKey, EvaluationRequestSha256: digest, MaximumDuration: time.Hour,
	}
	return settings, store, service, blobs
}

// Public discovery honestly retains all three host negatives and unavailable
// strict readiness while the distinct maintenance capability queues new work.
func TestCompetitionCalibrationMaintenanceComposesOfflineIntake(t *testing.T) {
	settings, store, service, _ := newCompetitionCalibrationMaintenanceFixture(t)
	ready, readyErr := service.Ready(context.Background())
	if readyErr == nil || ready.Ready || ready.RuntimeStatus == nil || !ready.RuntimeStatus.IntakeReady || ready.RuntimeStatus.GradingReady || !ready.RuntimeStatus.PublicationHeld {
		t.Fatalf("maintenance readiness conflated intake with grading: %+v %v", ready, readyErr)
	}
	for _, name := range []string{"authoritative_evaluator_host", "artifact_storage", "host_rebaseline"} {
		if ready.Checks[name] {
			t.Fatalf("maintenance fabricated %s", name)
		}
	}
	info, infoErr := service.Info(context.Background())
	if infoErr != nil || info.RuntimeStatus == nil || !info.RuntimeStatus.IntakeReady || info.RuntimeStatus.GradingReady || !info.RuntimeStatus.PublicationHeld {
		t.Fatalf("maintenance discovery failed: %+v %v", info, infoErr)
	}
	holds, err := store.AutomationHoldStatus(context.Background(), settings)
	if err != nil || service.intakeAuthority.holdsMatch(holds) {
		t.Fatal("maintenance replayed initial revision-one approval", err)
	}
	beforeReads := store.reader.reads
	accepted, status, submitErr := service.SubmitV2(context.Background(), ScoreArgs{RoundId: store.round.RoundId, Patch: testPatch("synthetic-maintenance-intake")}, &Principal{Id: "synthetic-submitter", Role: "submitter"})
	if submitErr != nil || status != http.StatusAccepted || accepted.State != "queued" || !store.maintenanceMarked || store.enqueueCalls != 1 || settings.intakeAdmission != nil {
		t.Fatalf("distinct maintenance permit did not queue: %+v %d %v", accepted, status, submitErr)
	}
	if store.reader.reads-beforeReads != 3 || store.reader.sourceReads != 2 {
		t.Fatalf("expected selection plus two latest-hold reads and two source reads: holds=%d source=%d", store.reader.reads-beforeReads, store.reader.sourceReads)
	}
	if store.enqueueJob.StartedAt != nil || store.enqueueJob.CompletedAt != nil || store.enqueueJob.AttemptCount != 0 || store.heartbeats != 0 || store.claims != 0 {
		t.Fatal("queuing maintenance intake started evaluation clocks or fabricated worker activity")
	}
}

// The hold's original column-backed creation time is inclusive; its single
// configured deadline is exclusive, and clock rollback never grants freshness.
func TestCompetitionCalibrationMaintenanceLifetimeIsBounded(t *testing.T) {
	settings, store, service, _ := newCompetitionCalibrationMaintenanceFixture(t)
	holds, err := store.AutomationHoldStatus(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	authority, createdAt := service.calibrationMaintenanceAuthority, holds.EvaluationHold.CreatedAt
	for _, candidate := range []struct {
		now     time.Time
		allowed bool
	}{
		{now: createdAt.Add(-time.Nanosecond), allowed: false},
		{now: createdAt, allowed: true},
		{now: createdAt.Add(authority.MaximumDuration - time.Nanosecond), allowed: true},
		{now: createdAt.Add(authority.MaximumDuration), allowed: false},
		{now: createdAt.Add(authority.MaximumDuration + time.Hour), allowed: false},
	} {
		if got := authority.holdsMatch(holds, candidate.now); got != candidate.allowed {
			t.Fatalf("lifetime boundary %s: got %t want %t", candidate.now, got, candidate.allowed)
		}
	}
	for _, duration := range []time.Duration{0, -time.Hour} {
		authority.MaximumDuration = duration
		if authority.holdsMatch(holds, createdAt) {
			t.Fatal("non-positive owner duration enabled maintenance intake")
		}
	}
}

// Future, historical, different-source and unapproved rounds retain ordinary
// host readiness; exact identity reuse does not weaken frozen-policy matching.
func TestCompetitionCalibrationMaintenanceRefusesAdjacentScopes(t *testing.T) {
	settings, store, service, _ := newCompetitionCalibrationMaintenanceFixture(t)
	frozen, err := settingsForAutomationRound(context.Background(), settings, store, store.round)
	if err != nil {
		t.Fatal(err)
	}
	holds, err := store.AutomationHoldStatus(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := service.readinessChecks(context.Background(), frozen)
	if err != nil {
		t.Fatal(err)
	}
	now := server.NowUtc()
	for index, change := range []func(*Settings, *roundRecord){
		func(s *Settings, _ *roundRecord) { s.AutomationPolicy.Environment = "synthetic-other-environment" },
		func(s *Settings, _ *roundRecord) { s.CompetitionId = "synthetic-other-competition" },
		func(s *Settings, _ *roundRecord) { s.AutomationPolicy.SeasonId = "synthetic-other-season" },
		func(s *Settings, _ *roundRecord) { s.AutomationPolicy.PolicyApprovalSha256 = strings.Repeat("4", 64) },
		func(s *Settings, _ *roundRecord) { s.AutomationPolicy.SourceLockSha256 = strings.Repeat("4", 64) },
		func(s *Settings, _ *roundRecord) { s.AutomationPolicy.SourceGeneration++ },
		func(s *Settings, _ *roundRecord) { s.AutomationPolicy.SourceActivationGeneration++ },
		func(s *Settings, _ *roundRecord) {
			s.AutomationPolicy.RequiredApiCapability = model.CompetitionAutomationCapability
		},
		func(s *Settings, _ *roundRecord) { s.EvaluatorImageDigest = "sha256:" + strings.Repeat("4", 64) },
		func(s *Settings, _ *roundRecord) { s.EvaluationPolicy.HardwareId = "synthetic-other-hardware" },
		func(s *Settings, _ *roundRecord) {
			s.EvaluationPolicy.HostQualificationSha256 = strings.Repeat("4", 64)
		},
		func(s *Settings, _ *roundRecord) { s.runtimeIntentSha256 = "" },
		func(_ *Settings, r *roundRecord) { r.RoundId = server.Id{8} },
		func(_ *Settings, r *roundRecord) { r.Epoch++ },
		func(_ *Settings, r *roundRecord) { r.Canceled = true },
		func(_ *Settings, r *roundRecord) { r.FinalizedAt = &now },
		func(_ *Settings, r *roundRecord) { r.AutomationSealedAt = &now },
		func(_ *Settings, r *roundRecord) { r.OpensAt = now.Add(time.Hour) },
		func(_ *Settings, r *roundRecord) { r.ClosesAt = now },
	} {
		candidate, round := *frozen, *store.round
		change(&candidate, &round)
		if service.approvedIntakeAdmission(&candidate, &round, checks, holds, now) != nil {
			t.Fatalf("adjacent scope %d acquired maintenance authority", index)
		}
	}
}

// A later release/rehold, incident or altered request cannot renew the fixed
// revision-four approval even with an otherwise fresh created-at timestamp.
func TestCompetitionCalibrationMaintenanceRejectsHoldDriftAndRenewal(t *testing.T) {
	settings, store, service, _ := newCompetitionCalibrationMaintenanceFixture(t)
	holds, err := store.AutomationHoldStatus(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	now := server.NowUtc()
	for index, change := range []func(*competitionAutomationHoldStatus){
		func(s *competitionAutomationHoldStatus) { s.Admission = true },
		func(s *competitionAutomationHoldStatus) { s.Evaluation = false },
		func(s *competitionAutomationHoldStatus) { s.RewardDelivery = false },
		func(s *competitionAutomationHoldStatus) { s.EvaluationHold = nil },
		func(s *competitionAutomationHoldStatus) { s.EvaluationHold.Revision++ },
		func(s *competitionAutomationHoldStatus) { s.EvaluationHold.Request.ExpectedRevision++ },
		func(s *competitionAutomationHoldStatus) { s.EvaluationHold.Request.Active = false },
		func(s *competitionAutomationHoldStatus) { s.EvaluationHold.Request.Kind = "admission" },
		func(s *competitionAutomationHoldStatus) { s.EvaluationHold.Request.ReasonCode = "OPERATOR_HOLD" },
		func(s *competitionAutomationHoldStatus) {
			s.EvaluationHold.Request.ReasonCode = "EVALUATION_INTEGRITY_INCIDENT"
		},
		func(s *competitionAutomationHoldStatus) {
			s.EvaluationHold.Request.IdempotencyKey = "synthetic-another-maintenance"
		},
		func(s *competitionAutomationHoldStatus) {
			s.EvaluationHold.Request.EvidenceSha256 = strings.Repeat("4", 64)
		},
		func(s *competitionAutomationHoldStatus) {
			s.EvaluationHold.Request.PublicExplanation = "Synthetic altered request."
		},
		func(s *competitionAutomationHoldStatus) { s.EvaluationHold.RequestSha256 = strings.Repeat("4", 64) },
		func(s *competitionAutomationHoldStatus) { s.EvaluationHold.CreatedAt = time.Time{} },
		func(s *competitionAutomationHoldStatus) { s.EvaluationHold.CreatedAt = now.Add(time.Hour) },
		func(s *competitionAutomationHoldStatus) { s.RewardDeliveryHold = nil },
		func(s *competitionAutomationHoldStatus) { s.RewardDeliveryHold.Revision++ },
		func(s *competitionAutomationHoldStatus) { s.RewardDeliveryHold.HoldId = server.Id{8} },
		func(s *competitionAutomationHoldStatus) { s.RewardDeliveryHold.Request.Active = false },
		func(s *competitionAutomationHoldStatus) { s.RewardDeliveryHold.RequestSha256 = strings.Repeat("4", 64) },
		func(s *competitionAutomationHoldStatus) {
			s.RewardDeliveryHold.Request.PublicExplanation = "Synthetic altered reward body."
		},
	} {
		candidate := holds
		evaluation, reward := *holds.EvaluationHold, *holds.RewardDeliveryHold
		candidate.EvaluationHold, candidate.RewardDeliveryHold = &evaluation, &reward
		change(&candidate)
		if service.calibrationMaintenanceAuthority.holdsMatch(candidate, now) {
			t.Fatalf("changed or renewed hold %d acquired maintenance authority", index)
		}
	}
}

// Unchanged metadata is not proof of unchanged reward authority. Both the pure
// permit selector and durable row reader authenticate the complete request body.
func TestCompetitionCalibrationMaintenanceRejectsRewardBodyTampering(t *testing.T) {
	settings, store, service, _ := newCompetitionCalibrationMaintenanceFixture(t)
	holds, err := store.AutomationHoldStatus(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	holds.RewardDeliveryHold.Request.PublicExplanation = "Synthetic reward body differs from its retained digest."
	if holds.RewardDeliveryHold.RequestSha256 != service.calibrationMaintenanceAuthority.Scope.RewardRequestSha256 {
		t.Fatal("fixture unexpectedly changed the pinned reward metadata")
	}
	if service.calibrationMaintenanceAuthority.holdsMatch(holds, server.NowUtc()) {
		t.Fatal("unchanged reward digest metadata authorized a tampered body")
	}
	reward := store.reader.records["reward_delivery"]["hold"].(*model.CompetitionAutomationHold)
	reward.Request.PublicExplanation = "Synthetic durable reward body differs from both digest copies."
	if _, err := store.AutomationHoldStatus(context.Background(), settings); err == nil {
		t.Fatal("matching row and JSON digest metadata concealed a changed reward body")
	}
}

// All non-host failures and missing dependencies remain hard admission gates,
// including a genuine protected archive check and authenticated API identity.
func TestCompetitionCalibrationMaintenancePreservesNonHostReadiness(t *testing.T) {
	_, store, service, _ := newCompetitionCalibrationMaintenanceFixture(t)
	for _, name := range []string{"configuration", "frozen_policy", "retention_window", "database", "fifo_slot", "queue_admission", "synthetic-unknown-dependency"} {
		store.fakeStore.readiness = map[string]bool{name: false}
		if _, status, err := service.SubmitV2(context.Background(), ScoreArgs{RoundId: store.round.RoundId, Patch: testPatch("synthetic-hard-gates")}, &Principal{Id: "synthetic-submitter", Role: "submitter"}); err == nil || status != http.StatusServiceUnavailable || store.enqueueCalls != 0 {
			t.Fatalf("maintenance waived %s", name)
		}
	}
	store.fakeStore.readiness = nil
	service.apiImageIdentityErr = errors.New("synthetic API identity unavailable")
	if _, status, err := service.SubmitV2(context.Background(), ScoreArgs{RoundId: store.round.RoundId, Patch: testPatch("synthetic-api-gate")}, &Principal{Id: "synthetic-submitter", Role: "submitter"}); err == nil || status != http.StatusServiceUnavailable || store.enqueueCalls != 0 {
		t.Fatal("maintenance waived API identity")
	}
}

// Maintenance authority never substitutes its approval digest for activated
// source/runtime authentication, retained protection, or submitter authority.
func TestCompetitionCalibrationMaintenancePreservesSourceArchiveAndAuth(t *testing.T) {
	_, store, service, blobs := newCompetitionCalibrationMaintenanceFixture(t)
	args, principal := ScoreArgs{RoundId: store.round.RoundId, Patch: testPatch("synthetic-maintenance-gates")}, &Principal{Id: "synthetic-submitter", Role: "submitter"}
	activatedAt := store.intent.ActivatedAt
	store.intent.ActivatedAt = nil
	if _, status, err := service.SubmitV2(context.Background(), args, principal); err == nil || status != http.StatusServiceUnavailable || store.enqueueCalls != 0 {
		t.Fatal("maintenance waived source activation")
	}
	store.intent.ActivatedAt = activatedAt
	blobs.protectionErr = errors.New("synthetic archive protection unavailable")
	if _, status, err := service.SubmitV2(context.Background(), args, principal); err == nil || status != http.StatusServiceUnavailable || store.enqueueCalls != 0 {
		t.Fatal("maintenance waived protected archive")
	}
	blobs.protectionErr = nil
	if _, status, err := service.SubmitV2(context.Background(), args, &Principal{Id: "synthetic-operator", Role: "operator"}); err == nil || status != http.StatusForbidden || store.enqueueCalls != 0 {
		t.Fatal("maintenance waived authenticated submitter role")
	}
}

// Column timestamps, hold identities and idempotency/digest facts independently
// constrain JSON, so a payload edit cannot extend maintenance authority.
func TestCompetitionCalibrationMaintenanceAuthenticatesDurableHoldFacts(t *testing.T) {
	for index, change := range []func(map[string]any){
		func(record map[string]any) { delete(record, "created_at") },
		func(record map[string]any) { record["created_at"] = server.NowUtc().Add(time.Hour) },
		func(record map[string]any) { record["hold_id"] = server.Id{8}.String() },
		func(record map[string]any) { record["idempotency_key"] = "synthetic-other-idempotency" },
		func(record map[string]any) { record["request_sha256"] = strings.Repeat("4", 64) },
		func(record map[string]any) {
			record["hold"].(*model.CompetitionAutomationHold).Request.PublicExplanation = "Synthetic JSON-only change."
		},
	} {
		settings, store, _, _ := newCompetitionCalibrationMaintenanceFixture(t)
		change(store.reader.records["evaluation"])
		if _, err := store.AutomationHoldStatus(context.Background(), settings); err == nil {
			t.Fatalf("malformed durable hold fact %d was accepted", index)
		}
	}
}

// The epoch deadline still uses original request receipt, but maintenance
// expiry uses a freshly sampled clock independently on every durable pass.
func TestCompetitionCalibrationMaintenanceSeparatesReceiptAndLeaseClocks(t *testing.T) {
	settings, store, service, _ := newCompetitionCalibrationMaintenanceFixture(t)
	frozen, err := settingsForAutomationRound(context.Background(), settings, store, store.round)
	if err != nil {
		t.Fatal(err)
	}
	frozen.intakeAdmission = &competitionIntakeAdmission{Authority: service.calibrationMaintenanceAuthority.Scope,
		MaintenanceAuthority: &service.calibrationMaintenanceAuthority, SourceIntentSha256: frozen.runtimeIntentSha256}
	store.round.OpensAt, store.round.ClosesAt = server.NowUtc().Add(-3*time.Hour), server.NowUtc().Add(-2*time.Hour)
	receivedAt := store.round.ClosesAt.Add(-time.Nanosecond)
	hold := store.reader.records["evaluation"]["hold"].(*model.CompetitionAutomationHold)
	now := hold.CreatedAt.Add(time.Minute)
	for pass := 0; pass < 2; pass++ {
		if err := checkCompetitionIntakeAdmissionWithClock(context.Background(), store.reader, frozen, store.round, receivedAt, func() time.Time { return now }); err != nil {
			t.Fatalf("on-time receipt rejected on pass %d: %v", pass, err)
		}
	}
	now = hold.CreatedAt.Add(service.calibrationMaintenanceAuthority.MaximumDuration)
	if err := checkCompetitionIntakeAdmissionWithClock(context.Background(), store.reader, frozen, store.round, receivedAt, func() time.Time { return now }); err == nil {
		t.Fatal("old receipt renewed expired maintenance authority")
	}
	if err := checkCompetitionIntakeAdmissionWithClock(context.Background(), store.reader, frozen, store.round, store.round.ClosesAt, func() time.Time { return hold.CreatedAt }); err == nil {
		t.Fatal("fresh maintenance authority admitted a late epoch receipt")
	}
}

// The archive barrier deterministically crosses the lease deadline; refusal
// occurs before a queued mutation even though receipt remains inside the epoch.
func TestCompetitionCalibrationMaintenanceExpiryDuringArchiveRefusesQueue(t *testing.T) {
	_, store, service, _ := newCompetitionCalibrationMaintenanceFixture(t)
	hold := store.reader.records["evaluation"]["hold"].(*model.CompetitionAutomationHold)
	now := hold.CreatedAt
	store.clock = func() time.Time { return now }
	status := competitionCalibrationMaintenanceArchiveRace(t, store, service, func() {
		now = hold.CreatedAt.Add(service.calibrationMaintenanceAuthority.MaximumDuration)
	})
	if status != http.StatusServiceUnavailable || store.enqueueCalls != 0 || store.reader.sourceReads != 2 {
		t.Fatalf("expired maintenance raced into queue: status=%d enqueues=%d source_reads=%d", status, store.enqueueCalls, store.reader.sourceReads)
	}
}

// The same latest-hold check runs after upload for release, rehold, incident,
// reward and admission changes, instead of trusting the minted capability.
func TestCompetitionCalibrationMaintenanceHoldChangesDuringArchiveRefuseQueue(t *testing.T) {
	for index, change := range []func(*competitionIntakeReadinessReader){
		func(reader *competitionIntakeReadinessReader) {
			hold := *reader.records["evaluation"]["hold"].(*model.CompetitionAutomationHold)
			hold.Revision, hold.Request.ExpectedRevision, hold.Request.Active, hold.Request.ReasonCode = 5, 4, false, "RECOVERY_APPROVED"
			reader.records["evaluation"] = competitionCalibrationMaintenanceChangedHold(&hold)
		},
		func(reader *competitionIntakeReadinessReader) {
			hold := *reader.records["evaluation"]["hold"].(*model.CompetitionAutomationHold)
			hold.Revision, hold.Request.ExpectedRevision, hold.Request.IdempotencyKey = 6, 5, "synthetic-maintenance-rehold"
			hold.CreatedAt = server.NowUtc()
			reader.records["evaluation"] = competitionCalibrationMaintenanceChangedHold(&hold)
		},
		func(reader *competitionIntakeReadinessReader) {
			hold := *reader.records["evaluation"]["hold"].(*model.CompetitionAutomationHold)
			hold.Revision, hold.Request.ExpectedRevision, hold.Request.ReasonCode = 5, 4, "EVALUATION_INTEGRITY_INCIDENT"
			reader.records["evaluation"] = competitionCalibrationMaintenanceChangedHold(&hold)
		},
		func(reader *competitionIntakeReadinessReader) {
			hold := *reader.records["reward_delivery"]["hold"].(*model.CompetitionAutomationHold)
			hold.Revision, hold.Request.ExpectedRevision, hold.Request.Active, hold.Request.ReasonCode = 2, 1, false, "RECOVERY_APPROVED"
			reader.records["reward_delivery"] = competitionCalibrationMaintenanceChangedHold(&hold)
		},
		func(reader *competitionIntakeReadinessReader) {
			hold := *reader.records["evaluation"]["hold"].(*model.CompetitionAutomationHold)
			hold.HoldId, hold.Request.Kind, hold.Request.ExpectedRevision, hold.Revision = server.Id{8}, "admission", 0, 1
			hold.Request.ReasonCode, hold.Request.IdempotencyKey = "OPERATOR_HOLD", "synthetic-admission-pause"
			reader.records["admission"] = competitionCalibrationMaintenanceChangedHold(&hold)
		},
	} {
		_, store, service, _ := newCompetitionCalibrationMaintenanceFixture(t)
		status := competitionCalibrationMaintenanceArchiveRace(t, store, service, func() { change(store.reader) })
		if status != http.StatusServiceUnavailable || store.enqueueCalls != 0 {
			t.Fatalf("hold race %d queued work: status=%d calls=%d", index, status, store.enqueueCalls)
		}
	}
}

// Durable source identity and incident state are reread on the existing
// transaction after upload, independent of earlier successful blob verification.
func TestCompetitionCalibrationMaintenanceSourceChangesDuringArchiveRefuseQueue(t *testing.T) {
	for index, change := range []func(*competitionIntakeReadinessReader){
		func(reader *competitionIntakeReadinessReader) { reader.sourceGeneration++ },
		func(reader *competitionIntakeReadinessReader) { reader.source.ActivatedAt = nil },
		func(reader *competitionIntakeReadinessReader) {
			now := server.NowUtc()
			reader.source.AbandonedAt = &now
		},
		func(reader *competitionIntakeReadinessReader) { reader.sourceInvalidated = true },
		func(reader *competitionIntakeReadinessReader) { reader.source.IntentSha256 = strings.Repeat("4", 64) },
		func(reader *competitionIntakeReadinessReader) {
			reader.source.Manifest = []byte("synthetic changed immutable manifest")
		},
		func(reader *competitionIntakeReadinessReader) { reader.source.TargetRoundId = server.Id{8} },
		func(reader *competitionIntakeReadinessReader) {
			reader.sourceErr = errors.New("synthetic source read unavailable")
		},
	} {
		_, store, service, _ := newCompetitionCalibrationMaintenanceFixture(t)
		status := competitionCalibrationMaintenanceArchiveRace(t, store, service, func() { change(store.reader) })
		if status != http.StatusServiceUnavailable || store.enqueueCalls != 0 || store.reader.sourceReads != 2 {
			t.Fatalf("source race %d queued work: status=%d calls=%d source_reads=%d", index, status, store.enqueueCalls, store.reader.sourceReads)
		}
	}
}

// A race after API selection but before the first durable pass is also refused;
// no retained upload is necessary to invalidate the request-local capability.
func TestCompetitionCalibrationMaintenanceRechecksBeforeFirstPass(t *testing.T) {
	for index, change := range []func(*competitionIntakeReadinessReader){
		func(reader *competitionIntakeReadinessReader) { reader.sourceGeneration++ },
		func(reader *competitionIntakeReadinessReader) { reader.sourceInvalidated = true },
		func(reader *competitionIntakeReadinessReader) {
			hold := *reader.records["evaluation"]["hold"].(*model.CompetitionAutomationHold)
			hold.Revision, hold.Request.ExpectedRevision = 5, 4
			reader.records["evaluation"] = competitionCalibrationMaintenanceChangedHold(&hold)
		},
	} {
		_, store, service, _ := newCompetitionCalibrationMaintenanceFixture(t)
		store.beforeAdmission = func() { change(store.reader) }
		if _, status, err := service.SubmitV2(context.Background(), ScoreArgs{RoundId: store.round.RoundId, Patch: testPatch("synthetic-first-pass-race")}, &Principal{Id: "synthetic-submitter", Role: "submitter"}); err == nil || status != http.StatusServiceUnavailable || store.enqueueCalls != 0 || !store.maintenanceMarked {
			t.Fatalf("first-pass race %d was accepted: status=%d err=%v", index, status, err)
		}
	}
}

// Ordinary all-green admission does not require this maintenance exception.
// An unapproved maintenance reason alone still cannot waive offline readiness.
func TestCompetitionCalibrationMaintenanceDoesNotGeneralizeOrdinaryReadiness(t *testing.T) {
	_, store, service, _ := newCompetitionCalibrationMaintenanceFixture(t)
	service.calibrationMaintenanceAuthority = competitionCalibrationMaintenanceAuthority{}
	args, principal := ScoreArgs{RoundId: store.round.RoundId, Patch: testPatch("synthetic-strict-readiness")}, &Principal{Id: "synthetic-submitter", Role: "submitter"}
	if _, status, err := service.SubmitV2(context.Background(), args, principal); err == nil || status != http.StatusServiceUnavailable || store.enqueueCalls != 0 {
		t.Fatal("an ordinary maintenance hold waived offline readiness")
	}
	store.fakeStore.readiness = map[string]bool{"authoritative_evaluator_host": true, "artifact_storage": true, "host_rebaseline": true}
	if _, status, err := service.SubmitV2(context.Background(), args, principal); err != nil || status != http.StatusAccepted || store.enqueueCalls != 1 || store.marked || store.reader.sourceReads != 0 {
		t.Fatalf("maintenance changed ordinary strict admission: status=%d err=%v", status, err)
	}
}

// A changed durable record carries its own authentic request digest, so race
// tests distinguish authority revocation from merely malformed JSON rejection.
func competitionCalibrationMaintenanceChangedHold(hold *model.CompetitionAutomationHold) map[string]any {
	hold.RequestSha256, _ = model.CompetitionAutomationDigest(hold.Request)
	return competitionIntakeReadinessHoldRecord(hold)
}

// Ordered barriers prove the final guard observes the change. An independent
// completion signal also joins cancellation/fatal cleanup after a result read.
func competitionCalibrationMaintenanceArchiveRace(t testing.TB, store *competitionIntakeReadinessStore, service *Service, change func()) int {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	store.archiveStart, store.archiveReturn = make(chan struct{}), make(chan struct{})
	archiveReturn := store.archiveReturn
	releaseArchive := sync.OnceFunc(func() { close(archiveReturn) })
	result := make(chan int, 1)
	finished := make(chan struct{})
	defer func() {
		cancel()
		releaseArchive()
		<-finished
	}()
	go func() {
		defer close(finished)
		_, status, _ := service.SubmitV2(ctx, ScoreArgs{RoundId: store.round.RoundId, Patch: testPatch("synthetic-maintenance-archive-race")}, &Principal{Id: "synthetic-submitter", Role: "submitter"})
		result <- status
	}()
	select {
	case <-store.archiveStart:
	case status := <-result:
		t.Fatalf("submission failed before maintenance archive barrier: %d", status)
	case <-ctx.Done():
		t.Fatal("maintenance archive barrier owner canceled")
	}
	func() {
		store.reader.stateLock.Lock()
		defer store.reader.stateLock.Unlock()
		change()
	}()
	releaseArchive()
	select {
	case status := <-result:
		return status
	case <-ctx.Done():
		t.Fatal("maintenance submission owner canceled")
		return 0
	}
}
