package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	// "github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

func BrevoWebhook(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(
		controller.BrevoWebhook,
		w,
		r,
	)
}
