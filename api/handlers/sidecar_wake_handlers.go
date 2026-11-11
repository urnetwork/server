package handlers

import (
	"net/http"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

// POST /sidecar/wake (§6.16, PROTO-M8-02): the bearer's sidecar summons another admitted device of its member, Client
// class on the strict_exp allowlist (api/route_authz.go). While the route is dark, MG1 not in the binary or
// sidecar.yml's wake switch off, it answers 503 with Retry-After before it reads the body or authenticates, as a
// closed gate does.
func SidecarWake(w http.ResponseWriter, r *http.Request) {
	if !controller.SidecarWakeOpen() {
		server.WriteGateClosed(w, 0)
		return
	}
	router.WrapWithInputRequireClient(controller.SidecarWake, w, r)
}
