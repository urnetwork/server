// A dedicated human capability imports only the exact retained model proposal.
package controller

import (
	"context"
	"github.com/urnetwork/server/model"
	"net/http"
)

type competitionReviewHandoffImportStore interface {
	ImportReviewHandoff(context.Context, *Settings, string, model.CompetitionReviewHandoffImport) (*model.CompetitionIntegrityReviewResult, error)
}

func CompetitionV2ReviewHandoffImportHandler(w http.ResponseWriter, r *http.Request) {
	competitionReviewHandoffImportHandler(DefaultService(), w, r)
}

func competitionReviewHandoffImportHandler(service *Service, w http.ResponseWriter, r *http.Request) {
	principal, ok := requireCompetitionV2Principal(w, r, service, competitionReviewOperatorRole)
	if !ok {
		return
	}
	var request model.CompetitionReviewHandoffImport
	if failure := decodeCompetitionBody(w, r, &request, 4096); failure != nil {
		writeCompetitionJson(w, http.StatusBadRequest, failure)
		return
	}
	if err := model.ValidateCompetitionReviewHandoffImport(request); err != nil {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_review_handoff_import", err.Error()))
		return
	}
	store, ok := service.store.(competitionReviewHandoffImportStore)
	if !ok {
		writeCompetitionJson(w, http.StatusServiceUnavailable, infrastructureError("capability_unavailable", "native review handoff import is unavailable"))
		return
	}
	result, err := store.ImportReviewHandoff(r.Context(), service.settings, principal.Id, request)
	if err != nil {
		status, failure := competitionAutomationError(err)
		writeCompetitionJson(w, status, failure)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeCompetitionJson(w, http.StatusOK, result)
}
