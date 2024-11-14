package handlers

import (
	"net/http"

	"github.com/urnetwork/server/bringyour/controller"
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/router"
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
