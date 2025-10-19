package handlers

import (
	"net/http"
	"time"

	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/router"
)

func NetworkCheck(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.NetworkCheck, w, r)
}

func NetworkCreate(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.NetworkCreate, w, r)
}

func UpdateNetworkName(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.UpdateNetworkName, w, r)
}

func GetNetworkReliability(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(
		router.CacheWithAuth(
			controller.GetNetworkReliability,
			"api_get_network_reliability",
			10*time.Second,
		),
		w,
		r,
	)
}
