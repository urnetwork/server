package handlers

import (
	"net/http"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// §8.17's api routes, Client class on the strict_exp allowlist
// (api/route_authz.go). POST /acl/invite and POST /acl/accept-invite land with
// cross-network shares (ACL-M6-03).

// POST /acl/ack: a client token only, the client that applied the version.
func AclAck(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(model.AclAck, w, r)
}
