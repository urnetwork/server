package handlers

import (
	"net/http"

	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/router"
)

func AccountPreferencesSet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.AccountPreferencesSet, w, r)
}

func AccountPreferencesGet(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.AccountPreferencesGet, w, r)
}
