package handlers

import (
	"net/http"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// §7.8's auth keys: the network credential lists, creates and revokes them
// (route_authz.go classes the three Network until TEAM-M6-03's roles).

func GetNetworkAuthKeys(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(model.GetNetworkAuthKeys, w, r)
}

func CreateNetworkAuthKey(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.CreateAuthKey, w, r)
}

func RevokeNetworkAuthKey(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.RevokeAuthKey, w, r)
}
