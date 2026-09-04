package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
)

// Competition endpoints use their own opaque, role-scoped bearer boundary;
// they intentionally do not pass through the consumer JWT wrappers.
func CompetitionHealth(w http.ResponseWriter, r *http.Request) {
	controller.HealthHandler(w, r)
}

func CompetitionReady(w http.ResponseWriter, r *http.Request) {
	controller.ReadyHandler(w, r)
}

func CompetitionInfo(w http.ResponseWriter, r *http.Request) {
	controller.InfoHandler(w, r)
}

func CompetitionLeaderboard(w http.ResponseWriter, r *http.Request) {
	controller.LeaderboardHandler(w, r)
}

func CompetitionGetRoundWorkload(w http.ResponseWriter, r *http.Request) {
	controller.GetRoundWorkloadHandler(w, r)
}

func CompetitionGenerateRound(w http.ResponseWriter, r *http.Request) {
	controller.GenerateRoundHandler(w, r)
}

// CompetitionGenerateStagingRound creates the one pre-season API-test round.
func CompetitionGenerateStagingRound(w http.ResponseWriter, r *http.Request) {
	controller.GenerateStagingRoundHandler(w, r)
}

func CompetitionSubmitScore(w http.ResponseWriter, r *http.Request) {
	controller.SubmitScoreHandler(w, r)
}

func CompetitionGetScore(w http.ResponseWriter, r *http.Request) {
	controller.GetScoreHandler(w, r)
}
