package handlers

import (
	"net/http"

    "bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/router"
)


func Hello(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.Hello, w, r)
}

