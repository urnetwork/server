package handlers

import (
	"net/http"

    "bringyour.com/bringyour/model"
	"bringyour.com/bringyour/router"
)


func GetNetworkLocations(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(model.GetActiveProviderLocations, w, r)
}


func NetworkLocations(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.FindActiveProviderLocations, w, r)
}


func NetworkActiveProviders(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.GetActiveProviders, w, r)
}

