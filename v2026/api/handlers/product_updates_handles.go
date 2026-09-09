package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	// "github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
)

func BrevoWebhook(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(
		controller.BrevoWebhook,
		w,
		r,
	)
}
