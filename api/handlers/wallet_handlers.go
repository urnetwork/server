package handlers

import (
	"net/http"

	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/router"
)

func WalletCircleInit(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.WalletCircleInit, w, r)
}

func WalletValidateAddress(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.WalletValidateAddress, w, r)
}

func WalletBalance(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.WalletBalance, w, r)
}

func WalletCircleTransferOut(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.WalletCircleTransferOut, w, r)
}
