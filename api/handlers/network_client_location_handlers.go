package handlers

import (
	"net/http"
	"time"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

func NetworkGetProviderLocations(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(router.CacheNoAuth(
		model.GetProviderLocations,
		"api_network_get_provider_locations",
		5*time.Second,
	), w, r)
}

func WarmNetworkGetProviderLocations(clientSession *session.ClientSession) {
	router.WarmCacheNoAuth(
		clientSession,
		model.GetProviderLocations,
		"api_network_get_provider_locations",
		5*time.Second,
	)
}

func NetworkFindProviderLocations(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.FindProviderLocations, w, r)
}

func NetworkFindLocations(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.FindLocations, w, r)
}

func NetworkFindProviders(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.FindProviders, w, r)
}

func NetworkFindProviders2(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.FindProviders2, w, r)
}

func NetworkCreateProviderSpec(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.CreateProviderSpec, w, r)
}
