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

func RemoveWallet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.RemoveWallet, w, r)
}

func CircleWebhook(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputBodyFormatterNoAuth(
		controller.VerifyCircleBody,
		controller.CircleWalletWebhook,
		w,
		r,
	)
}
