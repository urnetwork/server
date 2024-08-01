package handlers

import (
	"net/http"

	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/router"
)

func StatsProvidersOverviewLast90(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(model.StatsProvidersOverviewLast90, w, r)
}

func StatsProviders(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(model.StatsProviders, w, r)
}

func StatsProviderLast90(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.StatsProviderLast90, w, r)
}
