package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/router"
)

func AuthNetworkClient(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.AuthNetworkClient, w, r)
}

func RemoveNetworkClient(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.RemoveNetworkClient, w, r)
}

func RemoveNetwork(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.NetworkRemove, w, r)
}

func NetworkClients(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(model.GetNetworkClients, w, r)
}

func DeviceSetProvide(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.DeviceSetProvide, w, r)
}
