package handlers

import (
	"net/http"

	"github.com/urnetwork/server/bringyour/controller"
	"github.com/urnetwork/server/bringyour/router"
)

func GetNetworkUser(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkUser, w, r)
}
