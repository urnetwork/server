package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
)

func ConnectControl(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(controller.ConnectControl, w, r)
}
