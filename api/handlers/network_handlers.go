package handlers

import (
	"net/http"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
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

// The caller network's admission and ranking is cached per network inside the
// controller; the appearance histograms are read fresh on every call.
func GetProviderStatus(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetProviderStatus, w, r)
}
