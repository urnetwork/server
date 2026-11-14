package handlers

import (
	"net/http"
	// "time"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

func NetworkGetProviderLocations(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(model.GetProviderLocations, w, r)
}

// func WarmNetworkGetProviderLocations(clientSession *session.ClientSession) {
// 	router.WarmCacheNoAuth(
// 		clientSession,
// 		model.GetProviderLocations,
// 		"api_network_get_provider_locations",
// 		15*time.Second,
// 		true,
// 	)
// }

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

// Bind the router-owned appearance counter without replacing the HTTP
// request's cancellation and deadline.
func NetworkFindProviders2WithAppearances(appearances *model.ProviderAppearances) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		NetworkFindProviders2(w, r.WithContext(model.WithProviderAppearances(r.Context(), appearances)))
	}
}

// find-providers2 with the owners the API lifecycle supplies: the provider
// appearances its answers count toward (nil for none), and the platform's
// override of the apps' strict window handover, which every answer carries
// (controller/window_strict_handover_control.go). Only the clients that ask
// over http see the override: hosted devices discover in process
// (localclient), without it.
func NetworkFindProviders2WithOwners(appearances *model.ProviderAppearances, windowStrictHandover *controller.WindowStrictHandoverControl) http.HandlerFunc {
	findProviders2 := findProviders2WithWindowStrictHandover(model.FindProviders2, windowStrictHandover)
	return func(w http.ResponseWriter, r *http.Request) {
		if appearances != nil {
			r = r.WithContext(model.WithProviderAppearances(r.Context(), appearances))
		}
		router.WrapWithInputNoAuth(findProviders2, w, r)
	}
}

// The platform's override of the apps' strict window handover, as an answer
// carries it (controller.WindowStrictHandoverControl).
type windowStrictHandoverOverride interface {
	Override() (enabled bool, set bool)
}

// find, with the override on a copy of each answer: an answer of find is never
// changed, and one without an override is returned as it is.
func findProviders2WithWindowStrictHandover(
	find func(*model.FindProviders2Args, *session.ClientSession) (*model.FindProviders2Result, error),
	override windowStrictHandoverOverride,
) func(*model.FindProviders2Args, *session.ClientSession) (*model.FindProviders2Result, error) {
	return func(args *model.FindProviders2Args, clientSession *session.ClientSession) (*model.FindProviders2Result, error) {
		result, err := find(args, clientSession)
		if err != nil || result == nil {
			return result, err
		}
		enabled, set := override.Override()
		if !set {
			return result, nil
		}
		stamped := *result
		stamped.WindowStrictHandover = &enabled
		return &stamped, nil
	}
}
