package handlers

import (
	"net/http"

	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/router"
)

func GetAccountPayments(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkAccountPayments, w, r)
}
