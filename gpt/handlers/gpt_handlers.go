package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

func GptPrivacyPolicy(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.GptPrivacyPolicy, w, r)
}

func GptBeMyPrivacyAgent(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.GptBeMyPrivacyAgent, w, r)
}
