package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
)

func CreateApiKey(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.CreateApiKey, w, r)
}

func GetApiKeys(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetApiKeys, w, r)
}

func DeleteApiKey(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.DeleteApiKey, w, r)
}
