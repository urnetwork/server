package handlers

import (
	"net/http"

    "bringyour.com/bringyour/model"
	"bringyour.com/bringyour/router"
)


func NetworkLocations(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.FindActiveProviderLocations, w, r)
}


func NetworkActiveProviders(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.GetActiveProviders, w, r)
}

