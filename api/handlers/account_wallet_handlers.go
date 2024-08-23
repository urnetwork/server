package handlers

import (
	"net/http"

	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/router"
)

func CreateAccountWallet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.CreateAccountWalletExternal, w, r)
}

func GetAccountWallets(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetAccountWallets, w, r)
}
