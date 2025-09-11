package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	// "github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

func AccountPreferencesSet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.AccountPreferencesSet, w, r)
}

func AccountPreferencesGet(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.AccountPreferencesGet, w, r)
}
