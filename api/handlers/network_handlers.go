package handlers

import (
	"net/http"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
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
// controller; the appearance histograms are read fresh on every call. The
// query takes an optional client_id, one client of the caller's network, and
// an optional format, "full" (the default) or "compact". Refusals answer 200
// with `error.message`. Every 200 carries an ETag of its body, scoped to the
// caller's network and the query, and an If-None-Match that names it is
// answered 304 with no body.
func GetProviderStatus(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	args := &controller.GetProviderStatusArgs{
		ClientId: query.Get("client_id"),
		Format:   query.Get("format"),
	}
	scope := controller.ProviderStatusEntityScope(args)
	if args.Format == controller.ProviderStatusFormatCompact {
		router.WrapRequireAuth(
			func(clientSession *session.ClientSession) (*controller.GetProviderStatusCompactResult, error) {
				return controller.GetProviderStatusCompact(args, clientSession)
			},
			w,
			r,
			router.ConditionalJsonFormatter[*controller.GetProviderStatusCompactResult](w, r, scope),
		)
		return
	}
	router.WrapRequireAuth(
		func(clientSession *session.ClientSession) (*controller.GetProviderStatusResult, error) {
			return controller.GetProviderStatusWithArgs(args, clientSession)
		},
		w,
		r,
		router.ConditionalJsonFormatter[*controller.GetProviderStatusResult](w, r, scope),
	)
}
