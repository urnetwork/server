package router

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/urnetwork/server/session"
)

// A lag.dev guest's client token (lag.dev/PLAN.md A1) works only on the lag
// routes the guest needs. Every other route refuses it with 403 before its
// handler runs, whatever its class and whatever wrapper its handler uses, so
// the guest's account can administer nothing and buy nothing, and the
// connect transport refuses it too (session/AUTH.md §1).

const LagGuestCredentialRefusedMessage = "A lag.dev guest token works only on the lag.dev routes."

// RefuseLagGuestCredentials returns the route with a gate in front of its
// handler that answers 403 to a lag.dev guest's token. Every other request
// reaches the handler unchanged.
func RefuseLagGuestCredentials(route *Route) *Route {
	refused := *route
	refused.lagGuestRefused = true
	refused.handler = lagGuestGate(route.handler)
	return &refused
}

// RefusesLagGuestCredentials reports whether the route has the lag.dev guest
// gate.
func (self *Route) RefusesLagGuestCredentials() bool {
	return self.lagGuestRefused
}

func lagGuestGate(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if requestLagGuestCredential(r) {
			RaiseHttpError(fmt.Errorf("%d %s", http.StatusForbidden, LagGuestCredentialRefusedMessage), w)
			return
		}
		handler(w, r)
	}
}

// requestLagGuestCredential reports whether the request carries a verified
// lag.dev guest token. The claim is read unverified first, so any other
// credential costs one decode of its claims and no verification. A token that
// claims to be a guest's but fails verification is left to the handler, whose
// authentication refuses it as it would without the gate.
func requestLagGuestCredential(r *http.Request) bool {
	const bearerPrefix = "Bearer "
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, bearerPrefix) {
		return false
	}
	token := authorization[len(bearerPrefix):]
	if strings.HasPrefix(token, "urn_") {
		// an API key authenticates as its network
		return false
	}
	if !session.ByJwtLagGuestUnverified(token) {
		return false
	}
	byJwt, err := session.ParseByJwtForAudience(r.Context(), token, session.ByJwtAudienceApi)
	return err == nil && byJwt.LagGuest
}
