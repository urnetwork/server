package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/router"
)

func SetPayoutWallet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SetPayoutWallet, w, r)
}

func GetPayoutWallet(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetPayoutWallet, w, r)
}
