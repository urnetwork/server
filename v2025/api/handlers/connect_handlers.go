package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

func ConnectControl(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(controller.ConnectControl, w, r)
}
