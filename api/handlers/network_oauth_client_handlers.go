package handlers

import (
	"net/http"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// §7.8's network OAuth clients, a team's CI credentials (TEAM-M6-08), each the
// network credential's of an Owner or Admin (route_authz.go,
// route_permission.go). Each answers 503 until the binary links MG6.

// GET /network/oauth-clients: the team's clients, never a secret.
func GetNetworkOauthClients(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(model.GetNetworkOauthClients, w, r)
}

// POST /network/oauth-clients: a new client, its secret in this answer alone.
func CreateNetworkOauthClient(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.CreateNetworkOauthClient, w, r)
}

// POST /network/oauth-client-revoke: the client goes, and its live tokens stop.
func RevokeNetworkOauthClient(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.RevokeNetworkOauthClient, w, r)
}
