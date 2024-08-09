package handlers

import (
	"net/http"

	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/router"
)

func SetPayoutWallet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SetPayoutWallet, w, r)
}
