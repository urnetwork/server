package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
)

func GetNetworkUser(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkUser, w, r)
}

func UpgradeGuest(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.UpgradeFromGuest, w, r)
}

func UpgradeGuestExisting(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.UpgradeFromGuestExisting, w, r)
}
