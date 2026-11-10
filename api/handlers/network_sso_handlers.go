package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// POST /network/idp (§7.6, TEAM-M6-05): Network class, the Owner's by the role
// gate. The body's client secret is sealed at once and never logged: the
// router logs a body's type, never its value.
func CreateNetworkIdp(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.CreateNetworkIdp, w, r)
}

// GET /auth/sso/callback?state=&code=[&error=] (§7.6, TEAM-M6-05): Public,
// its credential the flow its state names, taken once.
func NetworkSsoCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	args := &controller.NetworkSsoCallbackArgs{
		State: query.Get("state"),
		Code:  query.Get("code"),
		Error: query.Get("error"),
	}
	impl := func(clientSession *session.ClientSession) (*controller.NetworkSsoCallbackResult, error) {
		return controller.NetworkSsoCallback(args, clientSession)
	}
	router.WrapNoAuth(impl, w, r)
}
