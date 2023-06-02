package handlers

import (
	"net/http"

    "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/router"
)


func AuthLogin(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(controller.AuthLogin, w, r)
}

func AuthLoginWithPassword(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(controller.AuthLoginWithPassword, w, r)
}

func AuthVerify(w http.ResponseWriter, r *http.Request) {
    router.WrapWithJson(model.AuthVerify, w, r)
}

func AuthVerifySend(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(controller.AuthVerifySend, w, r)
}

func AuthPasswordReset(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(controller.AuthPasswordReset, w, r)
}

func AuthPasswordSet(w http.ResponseWriter, r *http.Request) {
    router.WrapWithJson(controller.AuthPasswordSet, w, r)
}

func AuthNetworkCheck(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJsonIgnoreSession(model.NetworkCheck, w, r)
}

func AuthNetworkCreate(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(controller.NetworkCreate, w, r)
}
