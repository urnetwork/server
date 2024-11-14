package handlers

import (
	"net/http"

	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/router"
)

func AuthNetworkClient(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.AuthNetworkClient, w, r)
}

func RemoveNetworkClient(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.RemoveNetworkClient, w, r)
}

func NetworkClients(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(model.GetNetworkClients, w, r)
}

func DeviceSetProvide(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.DeviceSetProvide, w, r)
}
