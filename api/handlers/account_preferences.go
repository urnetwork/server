package handlers

import (
	"net/http"

	"github.com/urnetwork/server/bringyour/controller"
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/router"
)

func AccountPreferencesSet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.AccountPreferencesSet, w, r)
}

func AccountPreferencesGet(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.AccountPreferencesGet, w, r)
}
