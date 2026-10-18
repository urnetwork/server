// Source readiness acknowledges an actually installed immutable runtime manifest, never caller claims.
package controller

import (
	"context"
	"net/http"
	"strconv"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// Source capabilities are independent from reward election and settlement.
type competitionSourceReadinessStore interface {
	SourceActivation(context.Context, *Settings, int64) (*model.CompetitionSourceActivation, error)
	RecordSourceReadiness(context.Context, *Settings, model.CompetitionSourceReadiness) error
}

// The source publisher may request an acknowledgment but cannot supply Ready or the runtime digest.
func CompetitionV2SourceReadinessHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	principal, ok := requireCompetitionV2Principal(w, r, service, competitionSourcePromoterRole)
	if !ok {
		return
	}
	values := router.GetPathValues(r)
	if len(values) != 1 {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_generation", "source generation is missing"))
		return
	}
	generation, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || generation < 1 || strconv.FormatInt(generation, 10) != values[0] {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_generation", "source generation is malformed"))
		return
	}
	if r.ContentLength != 0 {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_readiness_request", "source readiness accepts no caller-supplied evidence"))
		return
	}
	settings, err := service.Settings()
	if err != nil {
		writeCompetitionJson(w, http.StatusServiceUnavailable, infrastructureError("configuration_unavailable", "competition configuration is not ready"))
		return
	}
	store, ok := service.store.(competitionSourceReadinessStore)
	if !ok {
		writeCompetitionJson(w, http.StatusServiceUnavailable, infrastructureError("capability_unavailable", "this replica does not support source readiness"))
		return
	}
	intent, err := store.SourceActivation(r.Context(), settings, generation)
	if err != nil {
		status, evalError := competitionAutomationError(err)
		writeCompetitionJson(w, status, evalError)
		return
	}
	if intent == nil || !competitionPrincipalAllows(principal, competitionSourcePromoterRole, intent.CompetitionId, model.CompetitionAutomationPolicy{Environment: intent.Environment, SeasonId: intent.SeasonId}) {
		writeCompetitionJson(w, http.StatusForbidden, competitionScopeDenied())
		return
	}
	if service.apiImageIdentityErr != nil || !imageDigestPattern.MatchString(service.apiImageDigest) || LoadCompetitionRuntimeManifest(settings, intent) != nil {
		writeCompetitionJson(w, http.StatusConflict, submissionError("source_not_ready", "the exact approved source manifest is not installed and verified on this API runtime"))
		return
	}
	receipt := model.CompetitionSourceReadiness{
		Staging:     intent.Staging,
		Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId,
		Generation: generation, TargetRoundId: intent.TargetRoundId, Role: "api", ActorId: principal.Id, ManifestCommit: intent.ManifestCommit,
		ManifestSha256: intent.ManifestSha256, SourceLockSha256: intent.SourceLockSha256,
		RuntimeImageDigest: service.apiImageDigest, QualificationSha256: intent.HostQualificationSha256,
		Ready: true, RecordedAt: server.NowUtc(),
	}
	if err := store.RecordSourceReadiness(r.Context(), settings, receipt); err != nil {
		status, evalError := competitionAutomationError(err)
		writeCompetitionJson(w, status, evalError)
		return
	}
	writeCompetitionJson(w, http.StatusOK, receipt)
}
