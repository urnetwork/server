package handlers

import (
	"net/http"
	"strings"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/apikey"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// §13.15's certificate routes (TLS-M5-16). The two order routes take a
// service token, aud certs, which the controller verifies; the bearer is no
// by_jwt, so no session authenticates there. The other two take a client
// token or a network credential; cert-revoke reads an API key's scopes here
// and passes them to the controller (QD-847).

// The request's bearer, empty for none.
func tlsBearer(r *http.Request) string {
	const bearerPrefix = "Bearer "
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, bearerPrefix) {
		return ""
	}
	return authorization[len(bearerPrefix):]
}

// POST /tls/cert-order
func TlsCertOrder(w http.ResponseWriter, r *http.Request) {
	token := tlsBearer(r)
	router.WrapWithInputNoAuth(
		func(args *sidecar.TlsCertOrderArgs, clientSession *session.ClientSession) (*sidecar.TlsCertOrderResult, error) {
			return controller.TlsCertOrder(clientSession.Ctx, token, args)
		},
		w,
		r,
	)
}

// GET /tls/cert-order/<id>
func GetTlsCertOrder(w http.ResponseWriter, r *http.Request) {
	token := tlsBearer(r)
	pathValues := router.GetPathValues(r)
	if len(pathValues) != 1 {
		http.Error(w, "Certificate not found.", http.StatusNotFound)
		return
	}
	certId, err := server.ParseId(pathValues[0])
	if err != nil || certId == (server.Id{}) {
		http.Error(w, "Certificate not found.", http.StatusNotFound)
		return
	}
	router.WrapNoAuth(
		func(clientSession *session.ClientSession) (*sidecar.TlsCertOrderStatus, error) {
			return controller.TlsCertOrderStatus(clientSession.Ctx, token, certId)
		},
		w,
		r,
	)
}

// GET /tls/certs
func GetTlsCerts(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.TlsCerts, w, r)
}

// The API key the session authenticated with, as its row reads now, for the
// controller's scope check (QD-847): nil for any other credential, and for a
// bearer that does not read as the session's key in the session's network,
// which then reaches nothing.
func tlsCertRevokeApiKey(clientSession *session.ClientSession, token string) *controller.TlsCertRevokeApiKey {
	if !clientSession.ApiKeyAuthenticated || clientSession.ApiKeyId == nil || clientSession.ByJwt == nil {
		return nil
	}
	byKey := apikey.GetNetworkByApiKey(token, clientSession.Ctx)
	if byKey == nil || byKey.ApiKeyId != *clientSession.ApiKeyId || byKey.NetworkId != clientSession.ByJwt.NetworkId {
		return nil
	}
	return &controller.TlsCertRevokeApiKey{ApiKeyId: byKey.ApiKeyId, Scopes: byKey.Scopes}
}

// POST /tls/cert-revoke
func TlsCertRevoke(w http.ResponseWriter, r *http.Request) {
	token := tlsBearer(r)
	router.WrapWithInputRequireAuth(
		func(args *sidecar.TlsCertRevokeArgs, clientSession *session.ClientSession) (*controller.TlsCertRevokeResult, error) {
			return controller.TlsCertRevoke(args, tlsCertRevokeApiKey(clientSession, token), clientSession)
		},
		w,
		r,
	)
}
