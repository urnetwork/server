package handlers

import (
	"net/http"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// FriendsApply is the public Friends of the Network application form: no
// authentication, rate limited with 429.
func FriendsApply(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.FriendsApply, w, r)
}
