package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
)

func AuthWalletChallenge(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.AuthWalletChallenge, w, r)
}
