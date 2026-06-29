package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

func AuthWalletChallenge(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.AuthWalletChallenge, w, r)
}
