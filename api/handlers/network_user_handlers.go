package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

func GetNetworkUser(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkUser, w, r)
}

func UpgradeGuest(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.UpgradeFromGuest, w, r)
}
