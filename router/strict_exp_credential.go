package router

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/urnetwork/server/session"
)

// A strict_exp token is a sidecar's: a client token that expires in 7 days and
// administers nothing (§7.4, D-TEAM-14). A fail-closed allowlist confines it:
// every route off the list refuses it before its handler runs, whatever its
// class and whatever wrapper its handler uses, and the routes that renew or mint
// also want a proof of the client's key. The answers are 403, not 401, so the
// sdk does not sign out (session/AUTH.md §4).
//
// The gates only ask. The api decides whether a strict_exp token is refused or
// only counted (api/route_authz.go), which sidecar.yml's p0_enforce gate moves.

const StrictExpCredentialRefusedMessage = "A sidecar token cannot call this route."

const StrictExpProofRefusedMessage = "A sidecar token needs a proof of its client key on this route."

// Decides whether a route refuses a verified strict_exp token's request. It sees
// the request, whose proof it reads, and the bearer token, which the proof binds.
type StrictExpProofRefusal func(r *http.Request, bearerToken string, byJwt *session.ByJwt) bool

// The route with a gate in front of its handler that answers 403 to a strict_exp
// token the refusal applies to. Every other request reaches the handler
// unchanged.
func RefuseStrictExpCredentials(route *Route, refusal ClientCredentialRefusal) *Route {
	refused := *route
	refused.strictExpRefusal = refusal
	refused.handler = strictExpGate(refusal, route.handler)
	return &refused
}

// Whether the route has the strict_exp gate: whether it is off the allowlist.
func (self *Route) RefusesStrictExpCredentials() bool {
	return self.strictExpRefusal != nil
}

// Serves the handler unless the refusal applies to a verified strict_exp token.
func strictExpGate(refusal ClientCredentialRefusal, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, byJwt := requestStrictExpCredential(r); byJwt != nil && refusal(r.Context(), byJwt) {
			RaiseHttpError(fmt.Errorf("%d %s", http.StatusForbidden, StrictExpCredentialRefusedMessage), w)
			return
		}
		handler(w, r)
	}
}

// The route with a gate in front of its handler that answers 403 to a strict_exp
// token's request the refusal applies to.
func RequireStrictExpProof(route *Route, refusal StrictExpProofRefusal) *Route {
	refused := *route
	refused.strictExpProofRefusal = refusal
	refused.handler = strictExpProofGate(refusal, route.handler)
	return &refused
}

// Whether the route has the proof gate.
func (self *Route) RequiresStrictExpProof() bool {
	return self.strictExpProofRefusal != nil
}

// Serves the handler unless the refusal applies to a verified strict_exp token's
// request.
func strictExpProofGate(refusal StrictExpProofRefusal, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if bearerToken, byJwt := requestStrictExpCredential(r); byJwt != nil && refusal(r, bearerToken, byJwt) {
			RaiseHttpError(fmt.Errorf("%d %s", http.StatusForbidden, StrictExpProofRefusedMessage), w)
			return
		}
		handler(w, r)
	}
}

// The request's strict_exp token, verified, with the bearer string it came from.
// Any other credential returns nil. The claim is read unverified first, so any
// other credential costs one decode of its claims and no verification. A token
// that claims to be strict_exp but fails verification is left to the handler,
// whose authentication refuses it as it would without the gate.
func requestStrictExpCredential(r *http.Request) (string, *session.ByJwt) {
	const bearerPrefix = "Bearer "
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, bearerPrefix) {
		return "", nil
	}
	token := authorization[len(bearerPrefix):]
	if strings.HasPrefix(token, "urn_") {
		// an API key authenticates as its network
		return "", nil
	}
	if !session.ByJwtStrictExpUnverified(token) {
		return "", nil
	}
	byJwt, err := session.ParseByJwtForAudience(r.Context(), token, session.ByJwtAudienceApi)
	if err != nil || !byJwt.StrictExp {
		return "", nil
	}
	return token, byJwt
}
