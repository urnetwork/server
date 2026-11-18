// Synthetic identities exercise the new authority boundaries without invoking
// a database, live artifact service, candidate program or host qualification.
package controller

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// These values are comparison fixtures, not approved grants or native receipts.
func competitionIntakeFirstTestValues() (*Settings, *model.CompetitionSourceActivation, *model.CompetitionSourceIntakeActivation, time.Time) {
	now := time.Date(2040, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := automationApiSettings()
	settings.AutomationPolicy.RequiredApiCapability = model.CompetitionRuntimeBlobCapability
	settings.IntakeFirstPolicy = &model.CompetitionIntakeFirstPolicy{Kind: model.CompetitionIntakeFirstKind, FromEpoch: 2, ApprovalSha256: strings.Repeat("a", 64)}
	settings.SeasonEndsAt, settings.RetainUntil = now.Add(60*24*time.Hour), now.Add(400*24*time.Hour)
	intent := &model.CompetitionSourceActivation{Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
		Generation: 2, ExpectedGeneration: 1, TargetEpoch: 2, TargetRoundId: server.NewId(), IntentSha256: strings.Repeat("b", 64),
		PreparedAt: now.Add(-4 * 24 * time.Hour), State: "prepared", Kind: "reviewed_source_carry_forward", Reason: "no_reward_winner"}
	grant := &model.CompetitionSourceIntakeActivation{Schema: 1, Kind: model.CompetitionIntakeFirstKind, Policy: *settings.IntakeFirstPolicy,
		Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId, Generation: intent.Generation, ExpectedGeneration: intent.ExpectedGeneration,
		TargetEpoch: intent.TargetEpoch, TargetRoundId: intent.TargetRoundId, IntentSha256: intent.IntentSha256, RoundPolicySha256: strings.Repeat("c", 64),
		ApiReadinessSha256: strings.Repeat("d", 64), ApiRuntimeImageDigest: "sha256:" + strings.Repeat("e", 64),
		SeasonEndsAt: settings.SeasonEndsAt, RetainUntil: settings.RetainUntil, CreatedAt: now.Add(-time.Hour)}
	return settings, intent, grant, now
}

// Long drain, build and old preparation times cannot backdate a new grant's
// actual opening or shorten its window. No submission clock exists in a grant.
func TestCompetitionIntakeFirstOpeningWaitsForActualPublication(t *testing.T) {
	settings, intent, grant, now := competitionIntakeFirstTestValues()
	args, err := competitionIntakeFirstOpening(settings, intent, grant, now)
	if err != nil || !args.OpensAt.Equal(now) || !args.ClosesAt.Equal(now.Add(7*24*time.Hour)) || !args.RevealAt.Equal(args.ClosesAt) {
		t.Fatal("late preparation did not preserve one actual full intake week", args, err)
	}
	// A later external archive completion chooses its own actual boundary;
	// CreateRound calls this only after that join and freezes it in the row.
	afterArchive := now.Add(19 * time.Minute)
	late, err := competitionIntakeFirstOpening(settings, intent, grant, afterArchive)
	if err != nil || !late.OpensAt.Equal(afterArchive) || late.ClosesAt.Sub(late.OpensAt) != 7*24*time.Hour {
		t.Fatal("archive delay consumed intake time", late, err)
	}
}

// Existing advertised reservations remain immutable even if an operator opts
// into the new capability later; the new path is explicitly ASAP, not rebooking.
func TestCompetitionIntakeFirstOpeningCannotMoveAnnouncedReservation(t *testing.T) {
	settings, intent, grant, now := competitionIntakeFirstTestValues()
	for _, reservation := range []time.Time{now.Add(-24 * time.Hour), now.Add(time.Hour)} {
		intent.OpensAt = reservation
		if _, err := competitionIntakeFirstOpening(settings, intent, grant, now); !errors.Is(err, ErrConflict) {
			t.Fatal("announced reservation was silently replaced", reservation, err)
		}
	}
}

// A full week is never truncated to fit fixed season/retention bounds. Changed
// deployment values cannot amend the bounds already pinned by the grant.
func TestCompetitionIntakeFirstOpeningRefusesSeasonOrRetentionExtension(t *testing.T) {
	settings, intent, grant, now := competitionIntakeFirstTestValues()
	settings.SeasonEndsAt = now.Add(7*24*time.Hour - time.Microsecond)
	grant.SeasonEndsAt = settings.SeasonEndsAt
	if _, err := competitionIntakeFirstOpening(settings, intent, grant, now); !errors.Is(err, ErrSeasonComplete) {
		t.Fatal("shortened week did not require an explicit amendment", err)
	}
	settings, intent, grant, now = competitionIntakeFirstTestValues()
	settings.SeasonEndsAt = settings.SeasonEndsAt.Add(time.Hour)
	if _, err := competitionIntakeFirstOpening(settings, intent, grant, now); !errors.Is(err, ErrConflict) {
		t.Fatal("mutable season setting replaced frozen grant bounds", err)
	}
	settings, intent, grant, now = competitionIntakeFirstTestValues()
	grant.RetainUntil = now
	if _, err := competitionIntakeFirstOpening(settings, intent, grant, now); err == nil {
		t.Fatal("missing immutable retention was accepted")
	}
}

// Target, policy, future-only era and permission time are all independent
// checks; a copied API digest alone cannot authorize another round or clock.
func TestCompetitionIntakeFirstOpeningRejectsForeignAuthority(t *testing.T) {
	for _, mutate := range []func(*Settings, *model.CompetitionSourceActivation, *model.CompetitionSourceIntakeActivation, *time.Time){
		func(_ *Settings, _ *model.CompetitionSourceActivation, grant *model.CompetitionSourceIntakeActivation, _ *time.Time) {
			grant.TargetRoundId = server.NewId()
		},
		func(_ *Settings, _ *model.CompetitionSourceActivation, grant *model.CompetitionSourceIntakeActivation, _ *time.Time) {
			grant.IntentSha256 = strings.Repeat("f", 64)
		},
		func(_ *Settings, intent *model.CompetitionSourceActivation, _ *model.CompetitionSourceIntakeActivation, _ *time.Time) {
			intent.TargetEpoch = 1
		},
		func(settings *Settings, _ *model.CompetitionSourceActivation, _ *model.CompetitionSourceIntakeActivation, _ *time.Time) {
			settings.AutomationPolicy.Staging = true
		},
		func(_ *Settings, _ *model.CompetitionSourceActivation, grant *model.CompetitionSourceIntakeActivation, _ *time.Time) {
			grant.Policy.ApprovalSha256 = strings.Repeat("f", 64)
		},
		func(_ *Settings, _ *model.CompetitionSourceActivation, grant *model.CompetitionSourceIntakeActivation, now *time.Time) {
			grant.CreatedAt = now.Add(time.Hour)
		},
		func(settings *Settings, _ *model.CompetitionSourceActivation, _ *model.CompetitionSourceIntakeActivation, _ *time.Time) {
			settings.SeasonPolicy.SubmissionWindowSeconds--
		},
	} {
		settings, intent, grant, now := competitionIntakeFirstTestValues()
		mutate(settings, intent, grant, &now)
		if _, err := competitionIntakeFirstOpening(settings, intent, grant, now); err == nil {
			t.Fatal("foreign or early opening authority was accepted")
		}
	}
}

// Staging an optional lifecycle capability must not add snapshot keys or alter
// any byte authenticated by the existing epoch-one secret/runtime contract.
func TestCompetitionIntakeFirstOptInDoesNotChangeLegacySnapshot(t *testing.T) {
	settings := automationApiSettings()
	before, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.IntakeFirstPolicy = &model.CompetitionIntakeFirstPolicy{Kind: model.CompetitionIntakeFirstKind, FromEpoch: 2, ApprovalSha256: strings.Repeat("a", 64)}
	after, err := policySnapshot(settings)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("prospective lifecycle policy changed historical snapshot bytes", err)
	}
	if competitionIntakeFirstNumericalPolicy(before) == nil {
		t.Fatal("new intake silently accepted a legacy scorer without the coverage contract")
	}
}

// One genuine API receipt is useful only to intake. Worker receipts, omitted
// recording time and changed source pins do not become substitute evidence.
func TestCompetitionIntakeFirstApiReceiptDoesNotInventWorkerAcknowledgment(t *testing.T) {
	_, intent, _, now := competitionIntakeFirstTestValues()
	intent.ManifestCommit, intent.ManifestSha256, intent.SourceLockSha256, intent.HostQualificationSha256 = strings.Repeat("a", 40), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
	api := competitionPreparationTestReceipt(intent, "api", "synthetic-api", testApiImageDigest())
	api.RecordedAt = now
	if _, err := competitionIntakeFirstApiReceiptDigest(intent, api); err != nil {
		t.Fatal(err)
	}
	if sourcePreparationReceiptsMatch(intent, []model.CompetitionSourceReadiness{api}) == nil {
		t.Fatal("API receipt alone became grading quorum")
	}
	for _, mutate := range []func(*model.CompetitionSourceReadiness){
		func(value *model.CompetitionSourceReadiness) { value.Role = "worker" },
		func(value *model.CompetitionSourceReadiness) { value.RecordedAt = time.Time{} },
		func(value *model.CompetitionSourceReadiness) { value.TargetRoundId = server.NewId() },
		func(value *model.CompetitionSourceReadiness) { value.ManifestSha256 = strings.Repeat("e", 64) },
	} {
		changed := api
		mutate(&changed)
		if _, err := competitionIntakeFirstApiReceiptDigest(intent, changed); err == nil {
			t.Fatal("another role or source acquired intake authority")
		}
	}
}

// Human review binds the product, including a separately approved instrument
// child, and supplements the actual provider/election record rather than
// relabeling the human as one of the frozen review models.
func TestCompetitionIntakeFirstHandoffBindsProductAndCurrentReview(t *testing.T) {
	_, intent, _, now := competitionIntakeFirstTestValues()
	job, review := server.NewId(), server.NewId()
	intent.Kind, intent.ReviewId, intent.RewardDecisionDigest, intent.SourceLockSha256 = "reviewed_source_promotion", &review, strings.Repeat("a", 64), strings.Repeat("b", 64)
	intent.ApprovedPatchSha256, intent.ReviewDecisionDigest = strings.Repeat("c", 64), strings.Repeat("d", 64)
	intent.WinnerReview = &model.CompetitionWinnerReviewCheckpoint{JobId: job}
	request := model.CompetitionSourceOperatorHandoffArgs{TargetEpoch: 2, RewardDecisionSha256: intent.RewardDecisionDigest, SelectedJobId: &job, ReviewId: &review,
		PatchSha256: intent.ApprovedPatchSha256, ReviewDecisionSha256: intent.ReviewDecisionDigest, ProductSourceLockSha256: intent.SourceLockSha256,
		Disposition: "approve_promotion", IdempotencyKey: "synthetic-handoff", EvidenceSha256: strings.Repeat("e", 64)}
	digest, err := model.CompetitionAutomationDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	handoff := &model.CompetitionSourceOperatorHandoff{Schema: 1, Kind: model.CompetitionSourceOperatorHandoffKind, Environment: intent.Environment, CompetitionId: intent.CompetitionId,
		SeasonId: intent.SeasonId, Revision: 1, ActorId: "synthetic-human", Request: request, RequestSha256: digest, CreatedAt: now}
	if !competitionSourceOperatorHandoffMatches(intent, handoff) {
		t.Fatal("exact positive operator handoff rejected")
	}
	intent.TrustedInstrument = &model.CompetitionTrustedInstrumentProof{SelectedProductKind: intent.Kind, SelectedProductSourceLockSha256: intent.SourceLockSha256}
	intent.SourceLockSha256 = strings.Repeat("f", 64)
	if !competitionSourceOperatorHandoffMatches(intent, handoff) {
		t.Fatal("instrument child was confused with human-approved product")
	}
	for _, mutate := range []func(*model.CompetitionSourceActivation){
		func(value *model.CompetitionSourceActivation) { value.RewardDecisionDigest = strings.Repeat("9", 64) },
		func(value *model.CompetitionSourceActivation) { value.ReviewDecisionDigest = strings.Repeat("9", 64) },
		func(value *model.CompetitionSourceActivation) { value.ApprovedPatchSha256 = strings.Repeat("9", 64) },
		func(value *model.CompetitionSourceActivation) { id := server.NewId(); value.ReviewId = &id },
	} {
		copy := *intent
		mutate(&copy)
		if competitionSourceOperatorHandoffMatches(&copy, handoff) {
			t.Fatal("changed elected/reviewed candidate reused an old human handoff")
		}
	}
	handoff.Request.Disposition = "reject_promotion"
	handoff.RequestSha256, _ = model.CompetitionAutomationDigest(handoff.Request)
	if competitionSourceOperatorHandoffMatches(intent, handoff) || !competitionSourceOperatorHandoffTargets(intent, handoff, false) {
		t.Fatal("latest rejection was ignored or could not be recorded")
	}
}

// No-winner carry is an explicit decision, not an implicit escape from a
// rejected, pending or replaced winner review.
func TestCompetitionIntakeFirstHandoffRequiresExplicitNoWinnerDisposition(t *testing.T) {
	_, intent, _, now := competitionIntakeFirstTestValues()
	intent.RewardDecisionDigest, intent.SourceLockSha256 = strings.Repeat("a", 64), strings.Repeat("b", 64)
	request := model.CompetitionSourceOperatorHandoffArgs{TargetEpoch: 2, RewardDecisionSha256: intent.RewardDecisionDigest,
		ProductSourceLockSha256: intent.SourceLockSha256, Disposition: "approve_no_winner_carry", IdempotencyKey: "synthetic-empty-ranking", EvidenceSha256: strings.Repeat("c", 64)}
	digest, _ := model.CompetitionAutomationDigest(request)
	handoff := &model.CompetitionSourceOperatorHandoff{Schema: 1, Kind: model.CompetitionSourceOperatorHandoffKind, Environment: intent.Environment,
		CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId, Revision: 1, ActorId: "synthetic-human", Request: request, RequestSha256: digest, CreatedAt: now}
	if !competitionSourceOperatorHandoffMatches(intent, handoff) {
		t.Fatal("explicit exhausted-ranking carry rejected")
	}
	for _, reason := range []string{"review_pending", "promotion_not_ready", "readiness_fallback"} {
		intent.Reason = reason
		if competitionSourceOperatorHandoffMatches(intent, handoff) {
			t.Fatal("non-final source choice bypassed the explicit human barrier", reason)
		}
	}
}

// An authenticated API-only grant may ignore only offline-host checks. Patch,
// image, archive, source, database and unknown checks remain fail-closed.
func TestCompetitionIntakeFirstReadinessDoesNotWaiveSecurityChecks(t *testing.T) {
	checks := map[string]bool{}
	for _, name := range []string{"configuration", "frozen_policy", "retention_window", "database", "fifo_slot", "queue_admission", "api_image_identity", "artifact_archive"} {
		checks[name] = true
	}
	checks["authoritative_evaluator_host"], checks["artifact_storage"], checks["host_rebaseline"] = false, false, false
	if !competitionIntakeChecksPass(checks) || secureEvaluatorChecksPass(checks) {
		t.Fatal("intake and actual evaluator readiness were conflated")
	}
	for _, name := range []string{"api_image_identity", "artifact_archive", "frozen_policy", "queue_admission", "unknown_security_check"} {
		checks[name] = false
		if competitionIntakeChecksPass(checks) {
			t.Fatal("security dependency was waived for intake", name)
		}
		delete(checks, name)
		if name != "unknown_security_check" {
			checks[name] = true
		}
	}
}

// The private status reader is the only source of grading-purpose projection.
type competitionIntakeFirstStatusTestStore struct {
	*fakeStore
	status *CompetitionSourceLifecycleStatus
	err    error
}

func (self *competitionIntakeFirstStatusTestStore) SourceLifecycleStatus(context.Context, *Settings, int64) (*CompetitionSourceLifecycleStatus, error) {
	return self.status, self.err
}

// A source pointer or a self-reported ready flag without the right purpose and
// exact grant cannot release public grading/publication while intake is open.
func TestCompetitionIntakeFirstRuntimeProjectionRequiresGradingPurpose(t *testing.T) {
	settings, _, grant, _ := competitionIntakeFirstTestValues()
	settings.intakeFirstActivation = grant
	store := &competitionIntakeFirstStatusTestStore{fakeStore: &fakeStore{}, status: &CompetitionSourceLifecycleStatus{Schema: 1, Capability: CompetitionSourceLifecycleCapability,
		Purpose: CompetitionSourceLifecycleIntake, Intake: grant, ApiAcknowledged: true, GradingRuntimeActivated: true}}
	service := &Service{settings: settings, store: store}
	checks := map[string]bool{}
	for _, name := range []string{"configuration", "frozen_policy", "retention_window", "database", "fifo_slot", "queue_admission", "api_image_identity", "artifact_archive"} {
		checks[name] = true
	}
	status, err := service.competitionRuntimeStatus(context.Background(), settings, nil, checks)
	if err != nil || !status.IntakeReady || status.GradingReady || !status.PublicationHeld {
		t.Fatal("API-only authority was reported as grading-ready", status, err)
	}
	store.status.Purpose, store.status.GradingReady = CompetitionSourceLifecycleGrading, true
	status, err = service.competitionRuntimeStatus(context.Background(), settings, nil, checks)
	if err != nil || status.GradingReady || !status.PublicationHeld {
		t.Fatal("bare grading label hid missing baseline/worker evidence", status, err)
	}
	store.status.Round = &CompetitionSourceLifecycleRound{RoundId: grant.TargetRoundId, Epoch: grant.TargetEpoch}
	store.status.WorkerAcknowledged, store.status.HostAdmissionReady, store.status.BaselineSha256 = true, true, strings.Repeat("f", 64)
	status, err = service.competitionRuntimeStatus(context.Background(), settings, nil, checks)
	if err != nil || !status.GradingReady || status.PublicationHeld {
		t.Fatal("authenticated grading-purpose reader did not release readiness", status, err)
	}
	store.err = errors.New("synthetic authority unavailable")
	if _, err := service.competitionRuntimeStatus(context.Background(), settings, nil, checks); err == nil {
		t.Fatal("failed authority read fabricated grading readiness")
	}
}

// Neither the absent preparation profile nor the old atomic cold-claim method
// can start a new intake-first candidate merely because a worker is launched.
func TestCompetitionIntakeFirstBaselineCannotStartCandidateAtomically(t *testing.T) {
	settings, _, grant, _ := competitionIntakeFirstTestValues()
	settings.intakeFirstActivation = grant
	store := &fakeStore{}
	worker := &Worker{settings: settings, store: store}
	if job, err := worker.claimWithPreparedRoundBaseline(context.Background(), HostSelfCheck{}); job != nil || err == nil || store.claims != 0 {
		t.Fatal("missing trusted baseline role fell through to a real claim", job, err, store.claims)
	}
	if job, err := (PostgresStore{}).ClaimPreparedRoundBaseline(context.Background(), settings, nil, nil); job != nil || !errors.Is(err, errCompetitionIntakeFirstPending) {
		t.Fatal("atomic legacy baseline/candidate path consumed a new clock", job, err)
	}
}
