package handlers

import (
	"net/http"

	"github.com/urnetwork/server/competition"
)

// Competition endpoints use their own opaque, role-scoped bearer boundary;
// they intentionally do not pass through the consumer JWT wrappers.
func CompetitionHealth(w http.ResponseWriter, r *http.Request) {
	competition.HealthHandler(w, r)
}

func CompetitionReady(w http.ResponseWriter, r *http.Request) {
	competition.ReadyHandler(w, r)
}

func CompetitionInfo(w http.ResponseWriter, r *http.Request) {
	competition.InfoHandler(w, r)
}

func CompetitionGetRoundWorkload(w http.ResponseWriter, r *http.Request) {
	competition.GetRoundWorkloadHandler(w, r)
}

func CompetitionGenerateRound(w http.ResponseWriter, r *http.Request) {
	competition.GenerateRoundHandler(w, r)
}

func CompetitionSubmitScore(w http.ResponseWriter, r *http.Request) {
	competition.SubmitScoreHandler(w, r)
}

func CompetitionGetScore(w http.ResponseWriter, r *http.Request) {
	competition.GetScoreHandler(w, r)
}
