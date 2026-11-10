package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// §7.6's domain routes (TEAM-M6-06), each the network credential's and the
// team's Owner's by the role gate (route_authz.go, route_permission.go). Each
// answers 503 until the binary links MG6, and 402 with a QuotaRefusal outside
// the team tier.

// POST /network/domain-add: adds a domain to verify and answers its TXT
// record.
func AddNetworkDomain(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.AddNetworkDomain, w, r)
}

// POST /network/domain-verify: looks the domain's TXT record up.
func VerifyNetworkDomain(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.VerifyNetworkDomain, w, r)
}

// POST /network/domain-remove: removes a domain, verified or not.
func RemoveNetworkDomain(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.RemoveNetworkDomain, w, r)
}

// POST /auth/sso-discover (§7.6, TEAM-M6-06): Public. A request that carries
// the caller's own sign-in is authenticated as any other, and that user is
// the one a new IdP identity may link to.
func NetworkSsoDiscover(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputOptionalAuth(controller.NetworkSsoDiscover, w, r)
}
