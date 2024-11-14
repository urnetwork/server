package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

func GetAccountPayments(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkAccountPayments, w, r)
}
