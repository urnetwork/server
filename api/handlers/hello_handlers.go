package handlers

import (
	"net/http"

	"github.com/urnetwork/server/bringyour/controller"
	"github.com/urnetwork/server/bringyour/router"
)

func Hello(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.Hello, w, r)
}
