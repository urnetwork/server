package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/router"
)

func Hello(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.Hello, w, r)
}
