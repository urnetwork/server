package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

// §16.6's server-gated approvals: POST /approval/cancel, Client class on the
// strict_exp allowlist (api/route_authz.go).

// POST /approval/cancel: an approver's ApprovalCancel and its decision.
func ApprovalCancel(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(controller.ApprovalCancel, w, r)
}
