package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2025/controller"
	// "github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/router"
)

func BrevoWebhook(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(
		controller.BrevoWebhook,
		w,
		r,
	)
}
