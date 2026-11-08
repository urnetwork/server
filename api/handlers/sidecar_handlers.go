package handlers

import (
	"net/http"
	"strconv"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// §5.15's sidecar identity routes, Client class on the strict_exp allowlist
// (api/route_authz.go).

// POST /sidecar/bind: a client token only, the client the binding names.
func SidecarBind(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(controller.SidecarBind, w, r)
}

// GET /sidecar/identity/<sidecar_id>[?before_generation=<g>].
func GetSidecarIdentity(w http.ResponseWriter, r *http.Request) {
	pathValues := router.GetPathValues(r)
	sidecarId, err := server.ParseId(pathValues[0])
	if err != nil {
		http.Error(w, "Invalid sidecar id.", http.StatusBadRequest)
		return
	}
	lookup := &controller.GetSidecarIdentityArgs{SidecarId: &sidecarId}
	if before := r.URL.Query().Get("before_generation"); before != "" {
		beforeGeneration, err := strconv.ParseInt(before, 10, 64)
		if err != nil || beforeGeneration < 1 {
			http.Error(w, "Invalid generation.", http.StatusBadRequest)
			return
		}
		lookup.BeforeGeneration = beforeGeneration
	}
	impl := func(clientSession *session.ClientSession) (*controller.GetSidecarIdentityResult, error) {
		return controller.GetSidecarIdentity(lookup, clientSession)
	}
	router.WrapRequireAuth(impl, w, r)
}

// GET /sidecar/identity?client_id=<id> (plan-CORE-client-to-sidecar-lookup).
func GetSidecarIdentityByClient(w http.ResponseWriter, r *http.Request) {
	clientId, err := server.ParseId(r.URL.Query().Get("client_id"))
	if err != nil {
		http.Error(w, "Invalid client id.", http.StatusBadRequest)
		return
	}
	lookup := &controller.GetSidecarIdentityArgs{ClientId: &clientId}
	impl := func(clientSession *session.ClientSession) (*controller.GetSidecarIdentityResult, error) {
		return controller.GetSidecarIdentity(lookup, clientSession)
	}
	router.WrapRequireAuth(impl, w, r)
}
