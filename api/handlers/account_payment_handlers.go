package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/router"
)

func GetAccountPayments(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkAccountPayments, w, r)
}
