package handlers

import (
	"net/http"

	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/router"
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
