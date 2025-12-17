package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/router"
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

func VerifyHoldingSeekerToken(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.VerifySeekerNftHolder, w, r)
}

func CircleWebhook(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputBodyFormatterNoAuth(
		controller.VerifyCircleBody,
		controller.CircleWalletWebhook,
		w,
		r,
	)
}
