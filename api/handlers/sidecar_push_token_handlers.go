package handlers

import (
	"net/http"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

// POST /sidecar/push-token (§18.7's step 0): the bearer's own client's wake token, Client class on the
// strict_exp allowlist (api/route_authz.go). While the route is dark, MG8 not in the binary or
// sidecar.yml's wake switch off, it answers 503 with Retry-After before it reads the body or
// authenticates, as a closed gate does. Nothing here logs the body (I-13): the router logs a body's size
// and type alone, and the controller's refusals never name the token.
func SidecarPushToken(w http.ResponseWriter, r *http.Request) {
	if !controller.SidecarPushTokenOpen() {
		server.WriteGateClosed(w, 0)
		return
	}
	router.WrapWithInputRequireClient(router.PrivateErrorsWithInput(controller.SidecarSetPushToken), w, r)
}
