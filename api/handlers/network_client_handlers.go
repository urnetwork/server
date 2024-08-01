package handlers

import (
	"net/http"

	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/router"
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
