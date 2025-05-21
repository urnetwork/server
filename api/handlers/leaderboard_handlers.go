package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

func GetLeaderboard(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.GetLeaderboard, w, r)
}

func GetLeaderboardNetworkRanking(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkLeaderboardRanking, w, r)
}

func SetNetworkLeaderboardPublic(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SetNetworkLeaderboardRankingPublic, w, r)
}
