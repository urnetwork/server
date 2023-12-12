package handlers

import (
	"net/http"

    "bringyour.com/bringyour/model"
	"bringyour.com/bringyour/router"
)


func NetworkGetActiveProviderLocations(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(model.GetActiveProviderLocations, w, r)
}


func NetworkFindActiveProviderLocations(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.FindActiveProviderLocations, w, r)
}


func NetworkFindLocations(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.FindLocations, w, r)
}


func NetworkFindActiveProviders(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.FindActiveProviders, w, r)
}

