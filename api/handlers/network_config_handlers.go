package handlers

import (
	"net/http"
	"strconv"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// §7.9's authorization log routes (TEAM-M2-13), Client class on the
// strict_exp allowlist (api/route_authz.go). Bodies are JSON with base64
// bytes, each signed message verbatim.

// POST /network/config/delta: one Signed statement, appended once verified at
// the tip.
func NetworkConfigDelta(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(controller.NetworkConfigDelta, w, r)
}

// POST /network/config/projection: a config writer's base, with its rows, and
// base 1 with its genesis.
func NetworkConfigProjectionPublish(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(controller.NetworkConfigProjectionPublish, w, r)
}

// GET /network/config/projection?after=: the entries after the position the
// caller holds.
func NetworkConfigProjection(w http.ResponseWriter, r *http.Request) {
	args := &controller.NetworkConfigProjectionArgs{}
	if value := r.URL.Query().Get("after"); value != "" {
		after, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			http.Error(w, "Invalid position.", http.StatusBadRequest)
			return
		}
		args.After = after
	}
	impl := func(clientSession *session.ClientSession) (*controller.NetworkConfigProjectionResult, error) {
		return controller.NetworkConfigProjection(args, clientSession)
	}
	router.WrapRequireClient(impl, w, r)
}

// POST /network/config/reseed: a member device's upload of the log it keeps,
// while sidecar.yml's reseed is true (§7.9's site loss, TEAM-M3-02).
func NetworkConfigReseed(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(model.PostConfigReseed, w, r)
}
