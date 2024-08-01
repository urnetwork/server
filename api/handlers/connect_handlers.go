package handlers

import (
	"net/http"

	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/router"
)

func ConnectControl(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(controller.ConnectControl, w, r)
}
