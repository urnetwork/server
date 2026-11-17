package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

// GET /sidecar/label (DESIGN.md 12.5; owner decision 421): the bearer's own network's label, allocated on
// first need, and its live alias, which a sidecar's NetworkLabelTable reads from M2. Client class on §7.4's
// strict_exp allowlist (api/route_authz.go). The network is the token's: the route reads no parameter and
// no body. An unexpected failure answers 503 with its cause kept in the log.
func GetSidecarLabel(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(router.PrivateErrors(controller.GetSidecarLabel), w, r)
}
