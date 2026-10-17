// Incident controls require independently scoped capabilities and never mutate historical evidence.
package controller

import (
	"context"
	"net/http"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Holds and incidents remain optional capabilities during a backward-compatible rollout.
type competitionAutomationIncidentStore interface {
	SetAutomationHold(context.Context, *Settings, string, model.CompetitionAutomationHoldArgs) (*model.CompetitionAutomationHold, error)
	RecordAutomationIncident(context.Context, *Settings, server.Id, string, model.CompetitionAutomationIncidentArgs) (*model.CompetitionAutomationIncident, error)
}

// Authenticates before decoding control requests, without promoting legacy operators.
func requireCompetitionV2ControlPrincipal(w http.ResponseWriter, r *http.Request, service *Service) (*Principal, bool) {
	settings, err := service.Settings()
	if err != nil {
		writeCompetitionJson(w, http.StatusServiceUnavailable, infrastructureError("configuration_unavailable", "competition configuration is not ready"))
		return nil, false
	}
	principal, ok := Authenticate(r, settings)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="competition"`)
		writeCompetitionJson(w, http.StatusUnauthorized, &CompetitionError{Kind: "auth", Code: "unauthorized", Message: "missing or invalid competition bearer token", Retriable: false})
		return nil, false
	}
	if principal.Role != competitionSchedulerRole && principal.Role != competitionSourcePromoterRole && principal.Role != competitionSettlementRole ||
		!competitionPrincipalAllows(principal, principal.Role, settings.CompetitionId, settings.AutomationPolicy) {
		writeCompetitionJson(w, http.StatusForbidden, competitionScopeDenied())
		return nil, false
	}
	return principal, true
}

// Each pause maps to exactly one capability; no broad operator or submitter fallback exists.
func competitionHoldRole(kind string) string {
	switch kind {
	case "admission", "evaluation":
		return competitionSchedulerRole
	case "source_activation":
		return competitionSourcePromoterRole
	case "reward_delivery":
		return competitionSettlementRole
	default:
		return ""
	}
}

// A pause never changes the accepted FIFO, source identity, or prior reward decisions.
func CompetitionV2HoldHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	principal, ok := requireCompetitionV2ControlPrincipal(w, r, service)
	if !ok {
		return
	}
	var args model.CompetitionAutomationHoldArgs
	if evalError := decodeCompetitionBody(w, r, &args, 16*1024); evalError != nil {
		writeCompetitionJson(w, http.StatusBadRequest, evalError)
		return
	}
	if err := model.ValidateCompetitionAutomationHold(args); err != nil {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_hold", err.Error()))
		return
	}
	if principal.Role != competitionHoldRole(args.Kind) {
		writeCompetitionJson(w, http.StatusForbidden, competitionScopeDenied())
		return
	}
	store, ok := service.store.(competitionAutomationIncidentStore)
	if !ok {
		writeCompetitionJson(w, http.StatusServiceUnavailable, infrastructureError("capability_unavailable", "this replica does not support automation holds"))
		return
	}
	result, err := store.SetAutomationHold(r.Context(), service.settings, principal.Id, args)
	if err != nil {
		status, evalError := competitionAutomationError(err)
		writeCompetitionJson(w, status, evalError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeCompetitionJson(w, http.StatusOK, result)
}

// Evaluation invalidation and source-compromise authority cannot be combined by one token.
func competitionIncidentRole(args model.CompetitionAutomationIncidentArgs) string {
	if args.InvalidatesEvaluation && !args.SourceCompromised {
		return competitionSchedulerRole
	}
	if args.SourceCompromised && !args.InvalidatesEvaluation {
		return competitionSourcePromoterRole
	}
	return ""
}

// Records the exact incident and triggers only its preauthorized safety consequences.
func CompetitionV2IncidentHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	principal, ok := requireCompetitionV2ControlPrincipal(w, r, service)
	if !ok {
		return
	}
	roundId, ok := competitionV2PathId(w, r)
	if !ok {
		return
	}
	var args model.CompetitionAutomationIncidentArgs
	if evalError := decodeCompetitionBody(w, r, &args, 16*1024); evalError != nil {
		writeCompetitionJson(w, http.StatusBadRequest, evalError)
		return
	}
	if err := model.ValidateCompetitionAutomationIncident(args); err != nil {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_incident", err.Error()))
		return
	}
	if principal.Role != competitionIncidentRole(args) {
		writeCompetitionJson(w, http.StatusForbidden, competitionScopeDenied())
		return
	}
	store, ok := service.store.(competitionAutomationIncidentStore)
	if !ok {
		writeCompetitionJson(w, http.StatusServiceUnavailable, infrastructureError("capability_unavailable", "this replica does not support automation incidents"))
		return
	}
	result, err := store.RecordAutomationIncident(r.Context(), service.settings, roundId, principal.Id, args)
	if err != nil {
		status, evalError := competitionAutomationError(err)
		writeCompetitionJson(w, status, evalError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeCompetitionJson(w, http.StatusOK, result)
}
