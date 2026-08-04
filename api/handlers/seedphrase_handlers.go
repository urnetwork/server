package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

func AuthRegenerateSeedphrase(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.RegenerateSeedphrase, w, r)
}

func AuthGenerateSeedphrase(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.GenerateSeedphrase, w, r)
}
