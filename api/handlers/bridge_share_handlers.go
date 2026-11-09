package handlers

import (
	"net/http"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// §17.16's share tables, Client class on the strict_exp allowlist
// (api/route_authz.go): a sidecar of either network reads both sealed tables
// and replaces its own network's at a higher version. POST /acl/invite and POST
// /acl/accept-invite, which file and bind a share, are ACL-M6-03's.

// GET /network/bridge/share/<share_id>.
func GetNetworkBridgeShare(w http.ResponseWriter, r *http.Request) {
	shareId, err := server.ParseId(router.GetPathValues(r)[0])
	if err != nil {
		http.Error(w, "Invalid share id.", http.StatusBadRequest)
		return
	}
	impl := func(clientSession *session.ClientSession) (*model.NetworkBridgeShareResult, error) {
		return model.GetNetworkBridgeShareForSidecar(shareId, clientSession)
	}
	router.WrapRequireClient(impl, w, r)
}

// PUT /network/bridge/share/<share_id>.
func PutNetworkBridgeShare(w http.ResponseWriter, r *http.Request) {
	shareId, err := server.ParseId(router.GetPathValues(r)[0])
	if err != nil {
		http.Error(w, "Invalid share id.", http.StatusBadRequest)
		return
	}
	impl := func(put *model.PutNetworkBridgeShareArgs, clientSession *session.ClientSession) (*model.NetworkBridgeShareResult, error) {
		return model.PutNetworkBridgeShareForSidecar(shareId, put, clientSession)
	}
	router.WrapWithInputRequireClient(impl, w, r)
}
