package router

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/urnetwork/server/v2026/session"
)

// A client token is the by_jwt of one client (it carries a client_id). The
// network credential is the network's root token or an API key, neither of
// which names a client. A route that administers the network or the account
// refuses a client token at the router, before its handler runs, whatever
// wrapper the handler uses (AUTHZ1.md).

const ClientCredentialRefusedMessage = "A client token cannot administer the network. Use the network's root token or an API key."

// ClientCredentialRefusal decides whether a route refuses a verified client
// token. It is called only for a token that names a client.
type ClientCredentialRefusal func(ctx context.Context, byJwt *session.ByJwt) bool

// RefuseEveryClientCredential refuses every client token.
func RefuseEveryClientCredential(ctx context.Context, byJwt *session.ByJwt) bool {
	return true
}

// RefuseClientCredentials returns the route with a gate in front of its
// handler that answers 403 to a client token the refusal applies to. Every
// other request reaches the handler unchanged, which authenticates it as
// before: a missing or invalid credential is still the handler's 401.
func RefuseClientCredentials(route *Route, refusal ClientCredentialRefusal) *Route {
	refused := *route
	refused.clientCredentialRefusal = refusal
	refused.handler = clientCredentialGate(refusal, route.handler)
	return &refused
}

// RefusesClientCredentials reports whether the route has a client token gate.
func (self *Route) RefusesClientCredentials() bool {
	return self.clientCredentialRefusal != nil
}

func (self *Route) Method() string {
	return self.method
}

func (self *Route) Pattern() string {
	return self.pattern
}

// Testing_WithHandler returns a copy of the route that serves handler behind
// the route's client token gate, so a test can observe what reaches it.
func Testing_WithHandler(route *Route, handler http.HandlerFunc) *Route {
	replaced := *route
	if route.clientCredentialRefusal != nil {
		replaced.handler = clientCredentialGate(route.clientCredentialRefusal, handler)
	} else {
		replaced.handler = handler
	}
	return &replaced
}

func clientCredentialGate(refusal ClientCredentialRefusal, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if byJwt := requestClientCredential(r); byJwt != nil && refusal(r.Context(), byJwt) {
			RaiseHttpError(fmt.Errorf("%d %s", http.StatusForbidden, ClientCredentialRefusedMessage), w)
			return
		}
		handler(w, r)
	}
}

// requestClientCredential returns the request's client token, verified. An
// API key, a network token, and a missing or invalid credential return nil.
// The claims are read unverified first, so a network credential costs no
// verification here; a token that names a client is verified before it can
// be refused. A token that fails verification is left to the handler, whose
// authentication refuses it exactly as it would without the gate.
func requestClientCredential(r *http.Request) *session.ByJwt {
	const bearerPrefix = "Bearer "
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, bearerPrefix) {
		return nil
	}
	token := authorization[len(bearerPrefix):]
	if strings.HasPrefix(token, "urn_") {
		// an API key authenticates as the network
		return nil
	}
	if !session.ByJwtNamesClientUnverified(token) {
		return nil
	}
	byJwt, err := session.ParseByJwtForAudience(r.Context(), token, session.ByJwtAudienceApi)
	if err != nil || byJwt.ClientId == nil {
		return nil
	}
	return byJwt
}
