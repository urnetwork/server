package handlers

import (
	"context"
	"net/http"
	"encoding/json"

	"bringyour.com/bringyour"
)


func AuthLogin(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(model.AuthLogin, w, r)
}

func AuthLoginWithPassword(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(controller.AuthLoginWithPassword, w, r)
}

func AuthValidate(w http.ResponseWriter, r *http.Request) {
    router.WrapWithJson(model.AuthValidate, w, r)
}

func AuthValidateSend(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(controller.AuthValidateSend, w, r)
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
