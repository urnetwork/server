// Explicit human source handoff is separate from automated provider review,
// source publication and independent API/worker readiness acknowledgments.
package controller

import (
	"context"
	"net/http"

	"github.com/urnetwork/server/v2026/model"
)

type competitionSourceHandoffStore interface {
	RecordSourceOperatorHandoff(context.Context, *Settings, string, model.CompetitionSourceOperatorHandoffArgs) (*model.CompetitionSourceOperatorHandoff, error)
}

// A dedicated scoped capability cannot be inherited by the publisher, reviewer,
// scheduler, submitter or historical unscoped operator token.
func CompetitionV2SourceHandoffHandler(w http.ResponseWriter, r *http.Request) {
	competitionSourceHandoffHandler(DefaultService(), w, r)
}

// The injected service is solely a test construction seam; production always
// delegates through the same authenticated default service above.
func competitionSourceHandoffHandler(service *Service, w http.ResponseWriter, r *http.Request) {
	principal, ok := requireCompetitionV2Principal(w, r, service, competitionSourceHandoffRole)
	if !ok {
		return
	}
	var request model.CompetitionSourceOperatorHandoffArgs
	if evalError := decodeCompetitionBody(w, r, &request, 16*1024); evalError != nil {
		writeCompetitionJson(w, http.StatusBadRequest, evalError)
		return
	}
	if err := model.ValidateCompetitionSourceOperatorHandoffArgs(request); err != nil {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_source_handoff", err.Error()))
		return
	}
	store, ok := service.store.(competitionSourceHandoffStore)
	if !ok {
		writeCompetitionJson(w, http.StatusServiceUnavailable, infrastructureError("capability_unavailable", "this replica does not support explicit source handoff"))
		return
	}
	value, err := store.RecordSourceOperatorHandoff(r.Context(), service.settings, principal.Id, request)
	if err != nil {
		status, evalError := competitionAutomationError(err)
		writeCompetitionJson(w, status, evalError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeCompetitionJson(w, http.StatusOK, value)
}
