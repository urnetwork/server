package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

func NetworkCheck(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.NetworkCheck, w, r)
}

func NetworkCreate(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.NetworkCreate, w, r)
}

func UpdateNetworkName(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.UpdateNetworkName, w, r)
}

func GetNetworkReliability(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkReliability, w, r)
}
