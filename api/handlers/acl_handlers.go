package handlers

import (
	"net/http"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// §8.17's api routes, Client class on the strict_exp allowlist
// (api/route_authz.go).

// POST /acl/ack: a client token only, the client that applied the version.
func AclAck(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(model.AclAck, w, r)
}

// POST /acl/invite: §8.11's invite, by a sidecar of the inviting network
// (ACL-M6-03).
func AclInvite(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(model.AclInvite, w, r)
}

// POST /acl/accept-invite: §8.11's claim or completion, by a sidecar of the
// invited network (ACL-M6-03).
func AclAcceptInvite(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(model.AclAcceptInvite, w, r)
}
