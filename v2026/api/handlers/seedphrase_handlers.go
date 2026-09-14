package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
)

func AuthRegenerateSeedphrase(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.RegenerateSeedphrase, w, r)
}

func AuthGenerateSeedphrase(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.GenerateSeedphrase, w, r)
}
