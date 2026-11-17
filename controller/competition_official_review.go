// An exact operator-deployed grant amends one official reviewer, not scores.
package controller

import (
	"context"
	"encoding/json"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"net/http"
)

type competitionOfficialReviewStore interface {
	ImportOfficialReview(context.Context, *Settings, string, model.CompetitionOfficialReviewImport) (*model.CompetitionIntegrityReviewResult, error)
}

// Authentication precedes decoding. A review_operator token alone cannot
// authorize substitution: the whole import must match a deployed exact grant.
func CompetitionV2OfficialReviewImportHandler(w http.ResponseWriter, r *http.Request) {
	competitionOfficialReviewImportHandler(DefaultService(), w, r)
}

func competitionOfficialReviewImportHandler(service *Service, w http.ResponseWriter, r *http.Request) {
	principal, ok := requireCompetitionV2Principal(w, r, service, competitionReviewOperatorRole)
	if !ok {
		return
	}
	var request model.CompetitionOfficialReviewImport
	if failure := decodeCompetitionBody(w, r, &request, 64*1024); failure != nil {
		writeCompetitionJson(w, http.StatusBadRequest, failure)
		return
	}
	digest, err := model.CompetitionAutomationDigest(request)
	if err != nil || service.settings.OfficialReviewImportSha256 == "" || digest != service.settings.OfficialReviewImportSha256 {
		writeCompetitionJson(w, http.StatusForbidden, competitionScopeDenied())
		return
	}
	store, ok := service.store.(competitionOfficialReviewStore)
	if !ok {
		writeCompetitionJson(w, http.StatusServiceUnavailable, infrastructureError("capability_unavailable", "official review import unavailable"))
		return
	}
	result, err := store.ImportOfficialReview(r.Context(), service.settings, principal.Id, request)
	if err != nil {
		status, failure := competitionAutomationError(err)
		writeCompetitionJson(w, status, failure)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeCompetitionJson(w, http.StatusOK, result)
}

// Shares the atomic evidence, current-election, review and decision insertion.
func (self PostgresStore) ImportOfficialReview(ctx context.Context, settings *Settings, actor string, request model.CompetitionOfficialReviewImport) (*model.CompetitionIntegrityReviewResult, error) {
	digest, err := model.CompetitionAutomationDigest(request)
	if err != nil || settings == nil || settings.OfficialReviewImportSha256 == "" || digest != settings.OfficialReviewImportSha256 || request.Review.OfficialReviewOverride == nil {
		return nil, ErrConflict
	}
	return self.recordIntegrityReview(ctx, settings, request.RoundId, actor, request.Review, nil, &request)
}

// The immutable authority and review commit together; replay keeps the original
// actor, bytes and time. An expired native owner is not touched or fabricated.
func (self PostgresStore) authorizeOfficialReviewInTx(ctx context.Context, tx server.PgTx, settings *Settings, actor string, request model.CompetitionOfficialReviewImport, policy model.CompetitionAutomationPolicy) error {
	if err := model.ValidateCompetitionOfficialReviewImport(request, policy); err != nil {
		return err
	}
	digest, err := model.CompetitionAutomationDigest(request)
	if err != nil || digest != settings.OfficialReviewImportSha256 || digest == "" {
		return ErrConflict
	}
	var epoch int
	var staging bool
	if err := tx.QueryRow(ctx, `SELECT epoch_number,staging FROM competition_round WHERE round_id=$1`, request.RoundId).Scan(&epoch, &staging); err != nil {
		return err
	}
	if epoch != 1 || staging {
		return ErrConflict
	}
	now, err := reviewHandoffDatabaseTime(ctx, tx)
	if err != nil {
		return err
	}
	if request.Review.OfficialReviewOverride.CompletedAt.After(now) {
		return ErrConflict
	}
	content, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('urnetwork.competition_official_review_actor',$1,true),set_config('urnetwork.competition_official_review_import_sha256',$2,true)`, actor, digest); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO competition_official_review_authorization(authorization_sha256,round_id,actor_id,import_sha256,import_json,created_at) VALUES($1,$2,$3,$4,$5::jsonb,$6) ON CONFLICT(authorization_sha256) DO NOTHING`, request.Review.OfficialReviewOverride.AuthorizationSha256, request.RoundId, actor, digest, string(content), now); err != nil {
		return err
	}
	var same bool
	if err := tx.QueryRow(ctx, `SELECT round_id=$2 AND actor_id=$3 AND import_sha256=$4 AND import_json=$5::jsonb FROM competition_official_review_authorization WHERE authorization_sha256=$1`, request.Review.OfficialReviewOverride.AuthorizationSha256, request.RoundId, actor, digest, string(content)).Scan(&same); err != nil {
		return err
	}
	if !same {
		return ErrConflict
	}
	return nil
}
