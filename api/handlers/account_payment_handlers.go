package handlers

import (
	"net/http"

	"github.com/urnetwork/server/bringyour/controller"
	"github.com/urnetwork/server/bringyour/router"
)

func GetAccountPayments(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkAccountPayments, w, r)
}
