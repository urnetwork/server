package handlers

import (
	"net/http"

    "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/router"
)


func AuthLogin(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.AuthLogin, w, r)
}


func AuthLoginWithPassword(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.AuthLoginWithPassword, w, r)
}


func AuthVerify(w http.ResponseWriter, r *http.Request) {
    router.WrapWithInputNoAuth(controller.AuthVerify, w, r)
}


func AuthVerifySend(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.AuthVerifySend, w, r)
}


func AuthPasswordReset(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.AuthPasswordReset, w, r)
}


func AuthPasswordSet(w http.ResponseWriter, r *http.Request) {
    router.WrapWithInputNoAuth(controller.AuthPasswordSet, w, r)
}


func AuthNetworkCheck(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.NetworkCheck, w, r)
}


func AuthNetworkCreate(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.NetworkCreate, w, r)
}


func AuthCodeCreate(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.AuthCodeCreate, w, r)
}


func AuthCodeLogin(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.AuthCodeLogin, w, r)
}
