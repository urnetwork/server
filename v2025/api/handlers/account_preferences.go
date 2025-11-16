package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2025/controller"
	// "github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/router"
)

func AccountPreferencesSet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.AccountPreferencesSet, w, r)
}

func AccountPreferencesGet(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.AccountPreferencesGet, w, r)
}
