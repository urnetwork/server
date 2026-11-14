// The composed offline actor has independent immutable archive versions and a
// one-shot transaction authority. It proves orchestration, not genuine models,
// real MinIO compliance, Docker containment or a production database deployment.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

func TestWinnerHandoffComposedPrepareFreezeCapturePendingAndProtectedValidate(t *testing.T) {
	bundle, draft, judgment := winnerMultipassFixture(t)
	request := bundle.Request.Request
	decision := model.CompetitionRewardDecision{RoundId: request.RoundId, Revision: request.RewardRevision, DecisionSha256: request.RewardDecisionSha256, SelectedJobId: &request.JobId, SealSha256: request.SealSha256}
	packet := *bundle.Request.Packet
	packet.RewardDecision, packet.WinnerReviewPolicy = &decision, model.CompetitionSnowWinnerReviewPolicy
	state := &controller.CompetitionWinnerReviewState{Policy: request.Policy, CurrentDecision: decision}
	release := winnerMultipassQualificationFixture(t).Release
	release.TokenizationProfiles = draft.Settings.TokenizationProfiles
	settings := defaultWinnerHandoffCaptureSettings()
	config := electedWinnerReviewHandoffConfig{RoundId: request.RoundId, JobId: request.JobId, RepositoriesRoot: t.TempDir(), AuthorizationSha256: winnerMultipassSha256([]byte("synthetic explicit user-choice record")),
		TimeoutSeconds: 3600, ProviderSettings: release.ProviderSettings, MultipassSettings: draft.Settings, RequiredSupplementalPaths: []string{}, UnresolvedContext: []string{},
		ActorId: "synthetic-pending-review-actor", Release: release, Qualification: currentQgQualificationPin{Path: "/synthetic/qualified-report", Sha256: winnerMultipassSha256([]byte("synthetic independently injected release proof"))},
		CaptureSettings: settings, StoragePaths: []string{"/synthetic/storage"}}
	insideTransaction := false
	checkOutsideTransaction := func() {
		t.Helper()
		if insideTransaction {
			t.Fatal("source/archive/release work entered the pure DB planner callback")
		}
	}
	sourceReads := 0
	source := winnerHandoffPreparationSource{
		Packet: func(context.Context) (*controller.CompetitionSourceReviewPacket, error) {
			checkOutsideTransaction()
			sourceReads++
			return &packet, nil
		},
		State: func(context.Context) (*controller.CompetitionWinnerReviewState, error) {
			checkOutsideTransaction()
			return state, nil
		},
		Files: func(context.Context, *controller.CompetitionSourceReviewPacket) ([]winnerReviewSourceFile, error) {
			checkOutsideTransaction()
			return bundle.Request.SourceFiles, nil
		},
		Workload: func(context.Context, *controller.CompetitionSourceReviewPacket) (*winnerWorkloadReviewContext, error) {
			checkOutsideTransaction()
			return bundle.Request.WorkloadReview, nil
		},
		Blob: func(context.Context, string, string, string) ([]byte, error) {
			t.Fatal("unexpected supplemental source read")
			return nil, errors.New("unexpected supplemental source")
		},
	}
	prepared, err := prepareElectedWinnerReviewWithSource(t.Context(), config, draft.Provider, source)
	if err != nil || prepared == nil || prepared.Status != "PREPARED_NO_MODEL_CALLS" || prepared.PromotionAuthorized {
		t.Fatal("composed exact candidate preparation failed", err)
	}
	layout, err := prepareWinnerMultipass(prepared.Bundle, draft.Owner, prepared.Settings, prepared.Provider)
	if err != nil {
		t.Fatal(err)
	}
	config.ScanGroups = layout.ScanGroups
	permissionArgs := prepared.Args
	permissionArgs.AuthorizationSha256 = ""
	releaseBytes, _ := json.Marshal(release)
	groupsBytes, _ := json.Marshal(config.ScanGroups)
	settingsBytes, _ := json.Marshal(settings)
	pathsBytes, _ := json.Marshal(config.StoragePaths)
	permission := winnerHandoffRunAuthorization{Schema: 1, Kind: "elected-winner-multipass-run-authorization/v1", UserApprovalSha256: config.AuthorizationSha256, AuthorizedAt: draft.Owner.StartedAt, ActorId: config.ActorId,
		Args: permissionArgs, ReleaseSha256: winnerMultipassSha256(releaseBytes), QualificationReportSha256: config.Qualification.Sha256, ScanGroupsSha256: winnerMultipassSha256(groupsBytes), CaptureSettingsSha256: winnerMultipassSha256(settingsBytes), StoragePathsSha256: winnerMultipassSha256(pathsBytes), SingleAttempt: true}
	permissionBytes, _ := json.Marshal(permission)
	config.RunAuthorization = currentQgQualificationPin{Path: "/synthetic/run-permission", Sha256: winnerMultipassSha256(permissionBytes)}
	archive := &winnerHandoffSpoolFixture{Commit: winnerHandoffSpoolCommit{MinimumUntil: draft.Owner.Deadline}, Protected: map[string][]byte{}}
	var durableOwner *model.CompetitionReviewHandoff
	freezeCalls, retainedPreparation, releaseChecks := 0, false, 0
	approved, rewardReleased, sourceGeneration := false, false, 1
	freezeAuthority := winnerHandoffFreezeAuthority{
		Now:           func() time.Time { return draft.Owner.StartedAt },
		VerifyRelease: func() error { checkOutsideTransaction(); releaseChecks++; return nil },
		Provider:      func() (winnerSnowProviderSettings, error) { checkOutsideTransaction(); return draft.Provider, nil },
		Prepare: func(ctx context.Context, provider winnerSnowProviderSettings) (*electedWinnerReviewPrepared, error) {
			checkOutsideTransaction()
			return prepareElectedWinnerReviewWithSource(ctx, config, provider, source)
		},
		ReadAuthorization: func() ([]byte, error) { checkOutsideTransaction(); return permissionBytes, nil },
		Retain: func(ctx context.Context, args model.CompetitionReviewHandoffArgs, content []byte) error {
			checkOutsideTransaction()
			if args.AuthorizationSha256 != config.RunAuthorization.Sha256 {
				return errors.New("prepared authority substitution")
			}
			_, err := archive.retain(ctx, content, winnerMultipassSha256(content))
			retainedPreparation = err == nil
			return err
		},
		Begin: func(ctx context.Context, args model.CompetitionReviewHandoffArgs, planner func(model.CompetitionReviewHandoff) (model.CompetitionReviewHandoffPlan, error)) (*model.CompetitionReviewHandoff, error) {
			if !retainedPreparation || durableOwner != nil {
				return nil, errors.New("synthetic transaction refuses missing source or repeated owner")
			}
			freezeCalls++
			owner := model.CompetitionReviewHandoff{HandoffId: server.NewId(), ActorId: config.ActorId, Args: args, StartedAt: draft.Owner.StartedAt, Deadline: draft.Owner.StartedAt.Add(time.Duration(args.TimeoutSeconds) * time.Second)}
			insideTransaction = true
			bound, err := planner(owner)
			insideTransaction = false
			if err != nil {
				return nil, err
			}
			if err := model.ValidateCompetitionReviewHandoffPlan(bound); err != nil {
				return nil, err
			}
			owner.Plan = bound
			durableOwner = &owner
			return &owner, nil
		},
	}
	frozen, err := freezeElectedWinnerReviewWithAuthority(t.Context(), config, prepared, freezeAuthority)
	if err != nil || frozen == nil || frozen.PromotionAuthorized || frozen.Status != "DB_FROZEN_NO_PROVIDER_CALLS_PENDING_CAPTURE_ADMISSION" || freezeCalls != 1 || sourceReads != 2 || releaseChecks != 1 {
		t.Fatal("composed freeze did not reproduce source then acquire one pending owner", err)
	}
	if !reflect.DeepEqual(frozen.Owner, durableOwner) || !frozen.Owner.StartedAt.Equal(draft.Owner.StartedAt) || !frozen.Owner.Deadline.Equal(draft.Owner.Deadline) {
		t.Fatal("freeze changed the independent immutable transaction clock")
	}
	ownerBytes, _ := json.Marshal(frozen.Owner)
	capacityBytes, _ := json.Marshal(frozen.Capacity)
	captureAdmission := winnerHandoffCaptureAdmission{Schema: 1, Kind: "elected-winner-frozen-capture-admission/v1", HandoffId: frozen.Owner.HandoffId, OwnerSha256: winnerMultipassSha256(ownerBytes), PlanSha256: frozen.Owner.Plan.PlanSha256,
		BundleSha256: frozen.Plan.BundleSha256, Provenance: frozen.Provenance, CapacitySha256: winnerMultipassSha256(capacityBytes), ResourceProofs: []currentQgQualificationPin{{Path: "/synthetic/resource-proof", Sha256: winnerMultipassSha256([]byte("synthetic independently injected resource proof"))}},
		AuthorizedAt: frozen.Owner.StartedAt, CompleteLeafPreflightRequired: true, NoRepackOrRenewal: true}
	admissionBytes, _ := json.Marshal(captureAdmission)
	if _, err := validateWinnerHandoffCaptureAdmission(admissionBytes, frozen.Owner, frozen.Plan, frozen.Bundle, frozen.Provenance, settings, frozen.Owner.StartedAt); err != nil {
		t.Fatal(err)
	}
	provenance := frozen.Provenance
	provenance.CaptureAdmissionSha256 = winnerMultipassSha256(admissionBytes)
	actor := &winnerHandoffFixture{owner: *frozen.Owner, plan: frozen.Plan, bundle: frozen.Bundle, now: frozen.Owner.StartedAt, claimed: map[string]bool{}}
	actor.evidence = winnerMultipassEvidenceFixture(t, frozen.Bundle, frozen.Plan, judgment)
	hooks := actor.hooks(t)
	hooks.Provenance = provenance
	hooks.Retain = archive.retain
	protect := func(ctx context.Context, value winnerHandoffInvocationCustody, transport []byte) winnerHandoffInvocationCustody {
		t.Helper()
		originals := map[string][]byte{"call.started.json": []byte("synthetic-started"), "call.complete.json": []byte("synthetic-completed"), "evidence-manifest.json": []byte("synthetic-wrapper-manifest"), "wrapper-receipt.json": []byte("synthetic-transport-proof"),
			"tokenize.request.json": []byte("synthetic-count-request"), "tokenize.response.json": []byte("synthetic-count-response"), "transport.response": transport}
		for index, original := range value.Originals {
			content, found := originals[original.Name]
			if !found || winnerMultipassSha256(content) != original.Retention.Sha256 {
				t.Fatal("synthetic original census does not reproduce declared bytes")
			}
			protected, err := retainWinnerHandoffOriginal(ctx, original.Name, content, settings.MaximumDocumentBytes, frozen.Owner.Deadline, archive.retain)
			if err != nil {
				t.Fatal(err)
			}
			value.Originals[index] = protected
		}
		return value
	}
	admit, invoke, claim := hooks.Admit, hooks.Invoke, hooks.Claim
	leafPreflights, expectedLeaves := 0, 0
	for _, call := range frozen.Plan.Calls {
		if call.Stage == "scan" {
			expectedLeaves++
		}
	}
	hooks.Admit = func(ctx context.Context, phase string, call winnerMultipassCall, payload []byte) (winnerHandoffTokenization, error) {
		if phase == "leaf_preflight" {
			leafPreflights++
		}
		value, err := admit(ctx, phase, call, payload)
		if err == nil {
			value.Custody = protect(ctx, value.Custody, nil)
		}
		return value, err
	}
	hooks.Claim = func(ctx context.Context, value model.CompetitionReviewHandoffCall) (*model.CompetitionReviewHandoffCall, error) {
		if leafPreflights != expectedLeaves {
			t.Fatal("generation claim preceded complete final frozen leaf preflight")
		}
		return claim(ctx, value)
	}
	hooks.Invoke = func(ctx context.Context, call winnerMultipassCall, payload []byte, admission winnerMultipassAdmission, claimSha string) (winnerHandoffTransport, error) {
		value, err := invoke(ctx, call, payload, admission, claimSha)
		if err == nil {
			value.Custody = protect(ctx, value.Custody, value.VisibleResponse)
		}
		return value, err
	}
	result, err := captureWinnerHandoff(t.Context(), filepath.Join(t.TempDir(), "capture"), frozen.Owner, frozen.Plan, frozen.Bundle, settings, hooks)
	if err != nil || result == nil || result.Status != "MODEL_FINDINGS_PENDING_OPERATOR" || result.PromotionAuthorized || result.Completion == nil || result.Completion.Proposal == nil || actor.invokes != len(frozen.Plan.Calls) {
		t.Fatal("composed model capture did not produce original retained pending findings", err)
	}
	validatedProtected := false
	pending := winnerHandoffPendingAuthority{
		Read: func(context.Context) (*model.CompetitionReviewHandoff, []model.CompetitionReviewHandoffCall, *model.CompetitionReviewHandoffCompletion, error) {
			return durableOwner, actor.journal, actor.lastCompletion, nil
		},
		State:      func(context.Context) (*controller.CompetitionWinnerReviewState, error) { return state, nil },
		Provenance: func(context.Context) (winnerHandoffProvenance, error) { return provenance, nil },
		ReloadProtected: func(ctx context.Context, calls []model.CompetitionReviewHandoffCall) error {
			content, _ := json.Marshal(result.Custody)
			original := winnerHandoffCustodyOriginal{Name: "custody.json", Bytes: int64(len(content)), Retention: result.Completion.Evidence}
			stored, err := archive.read(ctx, original)
			if err != nil || !bytes.Equal(stored, content) {
				return errors.New("pending protected completion changed")
			}
			for _, document := range result.Custody.Documents {
				if _, err := archive.read(ctx, document); err != nil {
					return err
				}
			}
			for _, invocation := range result.Custody.Invocations {
				for _, original := range invocation.Originals {
					if _, err := archive.read(ctx, original); err != nil {
						return err
					}
				}
			}
			validatedProtected = true
			return nil
		},
	}
	if err := validateWinnerHandoffPendingEvidence(t.Context(), frozen.Owner, frozen.Plan, frozen.Bundle, result.Evidence, result.Custody, result.Completion, pending); err != nil {
		t.Fatal("composed pending replay failed", err)
	}
	if !validatedProtected || approved || rewardReleased || sourceGeneration != 1 || state.ApprovedReviewId != nil || actor.completions != 1 {
		t.Fatal("pending validation performed or implied an operator/source/reward action")
	}
	// A missing exact original is a fail-closed replay, even though the pending
	// DB row, all model findings and original source still appear complete.
	lost := result.Custody.Invocations[0].Originals[0].Retention.VersionId
	delete(archive.Protected, lost)
	if err := validateWinnerHandoffPendingEvidence(t.Context(), frozen.Owner, frozen.Plan, frozen.Bundle, result.Evidence, result.Custody, result.Completion, pending); err == nil {
		t.Fatal("pending validation accepted a disappearing exact-version original")
	}
	if freezeCalls != 1 || actor.invokes != len(frozen.Plan.Calls) || approved || rewardReleased || sourceGeneration != 1 {
		t.Fatal("failed replay rerolled a clock/model call or crossed an approval boundary")
	}
}

func TestWinnerHandoffPreparationRejectsMissingAuthenticatedState(t *testing.T) {
	bundle, draft, _ := winnerMultipassFixture(t)
	config := electedWinnerReviewHandoffConfig{RoundId: bundle.Request.Request.RoundId, JobId: bundle.Request.Request.JobId, RepositoriesRoot: t.TempDir(), AuthorizationSha256: winnerMultipassSha256([]byte("synthetic preparation")),
		MultipassSettings: draft.Settings, RequiredSupplementalPaths: []string{}, UnresolvedContext: []string{}}
	for _, nilPacket := range []bool{false, true} {
		source := winnerHandoffPreparationSource{
			Packet: func(context.Context) (*controller.CompetitionSourceReviewPacket, error) {
				if nilPacket {
					return nil, nil
				}
				return bundle.Request.Packet, nil
			},
			State: func(context.Context) (*controller.CompetitionWinnerReviewState, error) { return nil, nil },
			Files: func(context.Context, *controller.CompetitionSourceReviewPacket) ([]winnerReviewSourceFile, error) {
				t.Fatal("missing authority reached source I/O")
				return nil, nil
			},
			Workload: func(context.Context, *controller.CompetitionSourceReviewPacket) (*winnerWorkloadReviewContext, error) {
				t.Fatal("missing authority reached instrument I/O")
				return nil, nil
			},
			Blob: func(context.Context, string, string, string) ([]byte, error) {
				t.Fatal("missing authority reached supplemental I/O")
				return nil, nil
			},
		}
		if _, err := prepareElectedWinnerReviewWithSource(t.Context(), config, draft.Provider, source); err == nil {
			t.Fatal("missing exact preparation authority was accepted")
		}
	}
}
