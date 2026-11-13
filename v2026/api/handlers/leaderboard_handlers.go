package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
)

func GetLeaderboard(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.GetLeaderboard, w, r)
}

func GetLeaderboardNetworkRanking(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkLeaderboardRanking, w, r)
}

func SetNetworkLeaderboardPublic(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SetNetworkLeaderboardRankingPublic, w, r)
}

// GetPointsLeaderboard is public; a signed-in caller also gets its own row.
func GetPointsLeaderboard(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputOptionalAuth(controller.GetPointsLeaderboard, w, r)
}

func SetNetworkPointsLeaderboardPublic(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SetNetworkPointsLeaderboardPublic, w, r)
}

func SetNetworkEmojiTag(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SetNetworkEmojiTag, w, r)
}
