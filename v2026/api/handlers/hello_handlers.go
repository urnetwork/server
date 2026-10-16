package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
)

func Hello(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.Hello, w, r)
}
