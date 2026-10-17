// V2 competition routes retain their own opaque, capability-scoped bearer boundary.
package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
)

// Screens a canonical patch for known reward exclusions before any fee collection.
func CompetitionV2Preflight(w http.ResponseWriter, r *http.Request) {
	controller.CompetitionV2PreflightHandler(w, r)
}

// Admits only a round that explicitly froze the v2 decision contract.
func CompetitionV2SubmitScore(w http.ResponseWriter, r *http.Request) {
	controller.CompetitionV2SubmitScoreHandler(w, r)
}

// Polls an owned immutable job without bypassing the publication embargo.
func CompetitionV2GetScore(w http.ResponseWriter, r *http.Request) {
	controller.CompetitionV2GetScoreHandler(w, r)
}

// Publishes a sealed evaluation separately from reward and source decisions.
func CompetitionV2RoundResult(w http.ResponseWriter, r *http.Request) {
	controller.CompetitionV2RoundResultHandler(w, r)
}

// Returns append-only reward history after the same publication gate as results.
func CompetitionV2Decisions(w http.ResponseWriter, r *http.Request) {
	controller.CompetitionV2DecisionsHandler(w, r)
}

// Elects once after durable close and drain with scheduler-only authority.
func CompetitionV2Seal(w http.ResponseWriter, r *http.Request) {
	controller.CompetitionV2SealHandler(w, r)
}

// Attributes bounded structured findings to the authenticated reviewer.
func CompetitionV2Review(w http.ResponseWriter, r *http.Request) {
	controller.CompetitionV2ReviewHandler(w, r)
}

// Serves durable ordered events only to the exact settlement integration scope.
func CompetitionV2Events(w http.ResponseWriter, r *http.Request) {
	controller.CompetitionV2EventsHandler(w, r)
}

// Acknowledges one exact event without implying payment or source approval.
func CompetitionV2Receipt(w http.ResponseWriter, r *http.Request) {
	controller.CompetitionV2ReceiptHandler(w, r)
}

// Records API readiness only for an installed exact source manifest.
func CompetitionV2SourceReadiness(w http.ResponseWriter, r *http.Request) {
	controller.CompetitionV2SourceReadinessHandler(w, r)
}

// Pauses exactly one independently authorized capability.
func CompetitionV2Hold(w http.ResponseWriter, r *http.Request) {
	controller.CompetitionV2HoldHandler(w, r)
}

// Records an append-only evaluation or source integrity incident.
func CompetitionV2Incident(w http.ResponseWriter, r *http.Request) {
	controller.CompetitionV2IncidentHandler(w, r)
}
