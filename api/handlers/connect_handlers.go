package handlers

import (
	"net/http"

	"github.com/urnetwork/server/bringyour/controller"
	"github.com/urnetwork/server/bringyour/router"
)

func ConnectControl(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(controller.ConnectControl, w, r)
}
