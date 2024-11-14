package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

func Hello(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.Hello, w, r)
}
