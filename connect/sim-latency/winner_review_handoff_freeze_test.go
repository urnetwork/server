// Freeze owns a prospective durable clock without calling a provider. All
// artifact loading, genuine qualification replay and authorization happen before
// the short lease/DB transaction; its planner performs only bounded pure work.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// This is an observation of a real DB-owned record, not a local checkpoint.
type winnerHandoffFrozen struct {
	Schema              int                             `json:"schema"`
	Status              string                          `json:"status"`
	PromotionAuthorized bool                            `json:"promotion_authorized"`
	Owner               *model.CompetitionReviewHandoff `json:"owner"`
	Plan                *winnerMultipassPlan            `json:"plan"`
	Bundle              winnerMultipassBundle           `json:"bundle"`
	Capacity            *winnerMultipassCapacity        `json:"capacity"`
	Provenance          winnerHandoffProvenance         `json:"provenance"`
}

// Exact replay returns the original owner/deadline; it never resets a spent,
// expired, superseded or previously legacy-owned review window.
func freezeElectedWinnerReview(ctx context.Context, config electedWinnerReviewHandoffConfig, prepared *electedWinnerReviewPrepared, control *controller.Settings, store controller.PostgresStore) (*winnerHandoffFrozen, error) {
	if prepared == nil || prepared.Schema != 1 || prepared.Status != "PREPARED_NO_MODEL_CALLS" || prepared.PromotionAuthorized || config.ActorId == "" || config.ScanGroups == nil || len(config.StoragePaths) == 0 ||
		config.CaptureSettings.FreezeLeaseDuration < 10*time.Second || config.CaptureSettings.FreezeLeaseDuration > 5*time.Minute || config.CaptureSettings.CleanupTimeout <= 0 || config.CaptureSettings.ReceiptTimeout <= 0 ||
		config.CaptureSettings.MaximumDocumentBytes <= 0 || config.CaptureSettings.MaximumDocumentBytes > defaultWinnerHandoffCaptureSettings().MaximumDocumentBytes || !reflect.DeepEqual(prepared.Settings.TokenizationProfiles, config.Release.TokenizationProfiles) {
		return nil, errors.New("freeze requires an authenticated prepared candidate, declared layout/resources and exact qualified runtime")
	}
	if err := authenticateWinnerMultipassReleaseArtifacts(config.Release); err != nil {
		return nil, err
	}
	if _, err := authenticateWinnerMultipassQualification(config.Qualification, config.Release); err != nil {
		return nil, err
	}
	provider, err := loadWinnerSnowProviderSettings(config.ProviderSettings.Path, config.ProviderSettings.Sha256)
	if err != nil || config.ProviderSettings != config.Release.ProviderSettings {
		return nil, errors.New("prepared provider does not match the qualified release")
	}
	fresh, err := prepareElectedWinnerReview(ctx, config, control, store, provider)
	if err != nil {
		return nil, err
	}
	wantBytes, _ := json.Marshal(prepared)
	actualBytes, _ := json.Marshal(fresh)
	if string(wantBytes) != string(actualBytes) {
		return nil, errors.New("prepared candidate no longer reproduces exact current source/election/settings")
	}
	authorityBytes, err := readCurrentQgQualificationPin(config.RunAuthorization, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true)
	if err != nil {
		return nil, err
	}
	var authority winnerHandoffRunAuthorization
	if decodeStrictJSONBytes(authorityBytes, &authority, "prospective run authority") != nil || authority.UserApprovalSha256 != prepared.Args.AuthorizationSha256 {
		return nil, errors.New("prospective runtime authority does not reference the prepared user-choice record")
	}
	args := prepared.Args
	args.AuthorizationSha256 = config.RunAuthorization.Sha256
	provenance, err := validateWinnerHandoffRunAuthorization(authorityBytes, config.RunAuthorization, config.ActorId, args, config.Release, config.Qualification, config.ScanGroups, config.CaptureSettings, config.StoragePaths, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	// Preserve authenticated source before owning a clock; this archive call is
	// outside the DB transaction and cannot invoke either reviewer.
	if _, err := controller.RetainCompetitionReviewHandoffEvidence(ctx, control, args.RoundId, args.JobId, wantBytes, winnerMultipassSha256(wantBytes)); err != nil {
		return nil, err
	}
	lease, err := store.AcquireAutomationLease(ctx, control, config.ActorId, config.CaptureSettings.FreezeLeaseDuration)
	if err != nil {
		return nil, err
	}
	var plan *winnerMultipassPlan
	var bundle winnerMultipassBundle
	var capacity *winnerMultipassCapacity
	owner, err := store.BeginReviewHandoff(ctx, control, *lease, config.ActorId, args, func(owner model.CompetitionReviewHandoff) (model.CompetitionReviewHandoffPlan, error) {
		// Pure callback: no tokenizer, files, archive, subprocess or other I/O.
		var err error
		plan, bundle, err = winnerHandoffPlan(owner, prepared.Bundle, prepared.Settings, prepared.Provider, config.ScanGroups)
		if err != nil {
			return model.CompetitionReviewHandoffPlan{}, err
		}
		capacity, err = estimateWinnerMultipassCapacity(plan, bundle, winnerHandoffTokenizerCensus(plan), config.CaptureSettings.Capacity)
		if err != nil || capacity.SerializedEvidenceBytes > int64(config.CaptureSettings.MaximumDocumentBytes) {
			return model.CompetitionReviewHandoffPlan{}, errors.New("fixed frozen plan exceeds its predeclared review resource/object bounds")
		}
		content, err := json.Marshal(plan)
		if err != nil {
			return model.CompetitionReviewHandoffPlan{}, err
		}
		bound := model.CompetitionReviewHandoffPlan{PlanSha256: winnerMultipassSha256(content), BundleSha256: plan.BundleSha256, CallIds: []string{}}
		for _, call := range plan.Calls {
			bound.CallIds = append(bound.CallIds, call.Id)
		}
		return bound, nil
	})
	if err != nil {
		return nil, err
	}
	if err := validateWinnerHandoffIdentity(owner, plan, bundle); err != nil {
		return nil, err
	}
	if !time.Now().UTC().Before(owner.Deadline) {
		return nil, errors.New("existing frozen owner is expired; no clock renewal is permitted")
	}
	return &winnerHandoffFrozen{Schema: 1, Status: "DB_FROZEN_NO_PROVIDER_CALLS_PENDING_CAPTURE_ADMISSION", Owner: owner, Plan: plan, Bundle: bundle, Capacity: capacity, Provenance: provenance}, nil
}
