package handlers

import (
	"context"
	"net/http"

    "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/router"
)


func AuthLogin(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(ctx, controller.AuthLogin, w, r)
}


func AuthLoginWithPassword(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(ctx, controller.AuthLoginWithPassword, w, r)
}


func AuthVerify(w http.ResponseWriter, r *http.Request) {
    router.WrapWithJson(ctx, model.AuthVerify, w, r)
}


func AuthVerifySend(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(ctx, controller.AuthVerifySend, w, r)
}


func AuthPasswordReset(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(ctx, controller.AuthPasswordReset, w, r)
}


func AuthPasswordSet(w http.ResponseWriter, r *http.Request) {
    router.WrapWithJson(ctx, controller.AuthPasswordSet, w, r)
}


func AuthNetworkCheck(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJsonIgnoreSession(ctx, model.NetworkCheck, w, r)
}


func AuthNetworkCreate(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(ctx, controller.NetworkCreate, w, r)
}
