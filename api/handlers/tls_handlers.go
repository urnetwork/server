package handlers

import (
	"net/http"
	"strings"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// §13.15's certificate routes (TLS-M5-16). The two order routes take a
// service token, aud certs, which the controller verifies; the bearer is no
// by_jwt, so no session authenticates there. The other two take a client
// token or a network credential.

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

// POST /tls/cert-revoke
func TlsCertRevoke(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.TlsCertRevoke, w, r)
}
