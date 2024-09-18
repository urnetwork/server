package handlers

import (
	"net/http"

	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/router"
)

func GetNetworkUser(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkUser, w, r)
}

func UpdateNetworkUser(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.UpdateNetworkUser, w, r)
}
