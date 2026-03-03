package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
)

func NetworkBlockLocation(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.NetworkBlockLocation, w, r)
}

func NetworkUnblockLocation(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.NetworkUnblockLocation, w, r)
}

func GetNetworkBlockedLocations(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkBlockedLocations, w, r)
}
