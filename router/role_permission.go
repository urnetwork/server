package router

import (
	"fmt"
	"net/http"
)

// §7.5's roles (D-TEAM-3, TEAM-M6-03): a team member's credential reaches a
// network or account administration route only where its member's role, and
// any scope it carries, holds the route's permission. The gate only asks: the
// api decides, from the route's permission and the request's credential
// (api/route_permission.go), and a credential of a personal network's owner,
// or none, is the api's to pass. The answer is 403, never 401, so a client does
// not sign out: the credential is good, its role is not enough.

const RolePermissionRefusedMessage = "Your role in this network does not permit this route."

// Decides whether a route refuses a request for its credential's role.
type RolePermissionRefusal func(r *http.Request) bool

// The route with a gate in front of its handler that answers 403 to a request
// the refusal applies to. Every other request reaches the handler unchanged.
func RequireRolePermission(route *Route, refusal RolePermissionRefusal) *Route {
	gated := *route
	gated.rolePermissionRefusal = refusal
	gated.handler = rolePermissionGate(refusal, route.handler)
	return &gated
}

// Whether the route has the role gate.
func (self *Route) RequiresRolePermission() bool {
	return self.rolePermissionRefusal != nil
}

// Serves the handler unless the refusal applies to the request.
func rolePermissionGate(refusal RolePermissionRefusal, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if refusal(r) {
			RaiseHttpError(fmt.Errorf("%d %s", http.StatusForbidden, RolePermissionRefusedMessage), w)
			return
		}
		handler(w, r)
	}
}
