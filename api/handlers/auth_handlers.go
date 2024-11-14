package handlers

import (
	"io"
	"net/http"

	"github.com/golang/glog"

	"github.com/urnetwork/server/bringyour/controller"
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/router"
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

func AuthConnect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, controller.SsoRedirectUrl(), http.StatusSeeOther)
}

func AppleNotification(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		glog.Infof("[apple]notification error = %s\n", err)
		http.Error(w, "Could not read notification.", http.StatusInternalServerError)
		return
	}
	glog.Infof("[apple]notification: %s\n", string(bodyBytes))

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{}"))
}
