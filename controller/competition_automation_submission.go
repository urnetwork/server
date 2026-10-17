// Authenticated v2 screening runs before fees; durable admission repeats the same check.
package controller

import (
	"context"
	"net/http"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Separate optional capability makes mixed-version replicas explicitly unavailable.
type competitionAutomationSubmissionStore interface {
	ScreenAutomationSubmission(context.Context, *Settings, server.Id, *CanonicalPatch) (bool, string, error)
}

// Returns canonical identity and known reward exclusion without charging or enqueuing.
func (self *Service) PreflightV2(ctx context.Context, args ScoreArgs, principal *Principal) (*model.CompetitionV2SubmissionPreflight, int, *CompetitionError) {
	if principal == nil || principal.Role != "submitter" {
		return nil, http.StatusForbidden, competitionScopeDenied()
	}
	settings, err := self.Settings()
	if err != nil {
		return nil, http.StatusServiceUnavailable, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	round, err := self.store.GetRound(ctx, settings, args.RoundId)
	if err != nil {
		status, evalError := competitionAutomationError(err)
		return nil, status, evalError
	}
	if status, evalError := requireRoundDecisionContract(round, true); evalError != nil {
		return nil, status, evalError
	}
	policy, err := decodeRoundPolicySnapshot(round.PolicyJson)
	if err != nil || policy.AutomationPolicy == nil {
		return nil, http.StatusServiceUnavailable, infrastructureError("round_policy_mismatch", "round automation policy could not be authenticated")
	}
	patch, evalError := ValidateAndCanonicalizePatch(args.Patch, policy.PatchPolicy)
	if evalError != nil {
		status := http.StatusUnprocessableEntity
		if evalError.Code == "patch_too_large" {
			status = http.StatusRequestEntityTooLarge
		}
		return nil, status, evalError
	}
	store, ok := self.store.(competitionAutomationSubmissionStore)
	if !ok {
		return nil, http.StatusServiceUnavailable, infrastructureError("capability_unavailable", "this replica does not support v2 submission screening")
	}
	eligible, reason, err := store.ScreenAutomationSubmission(ctx, settings, args.RoundId, patch)
	if err != nil {
		status, evalError := competitionAutomationError(err)
		return nil, status, evalError
	}
	return &model.CompetitionV2SubmissionPreflight{
		RoundId: args.RoundId, PatchSha256: patch.Sha256, BaseSourceLockSha256: policy.AutomationPolicy.SourceLockSha256,
		DecisionMode: model.CompetitionDecisionModeV2, RequiredApiCapability: model.CompetitionAutomationCapability,
		RewardEligible: eligible, IneligibilityReason: reason,
	}, http.StatusOK, nil
}

// Screening uses the same bounded patch body and authenticated submitter boundary as admission.
func CompetitionV2PreflightHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	principal, ok := requirePrincipal(w, r, service, false)
	if !ok {
		return
	}
	if principal.Role != "submitter" {
		writeCompetitionJson(w, http.StatusForbidden, competitionScopeDenied())
		return
	}
	var args ScoreArgs
	// The requested round's frozen patch limit is applied after its identity is decoded.
	if evalError := decodeCompetitionBody(w, r, &args, int64(hardRequestBodyLimit)); evalError != nil {
		writeCompetitionJson(w, http.StatusBadRequest, evalError)
		return
	}
	result, status, evalError := service.PreflightV2(r.Context(), args, principal)
	if evalError != nil {
		writeCompetitionJson(w, status, evalError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeCompetitionJson(w, status, result)
}
