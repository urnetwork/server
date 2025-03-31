package handlers

import (
	"net/http"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

func NetworkGetProviderLocations(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(model.GetProviderLocations, w, r)
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
