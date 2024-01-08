package handlers

import (
	"net/http"

	"bringyour.com/bringyour/router"
	"bringyour.com/bringyour/controller"
)


func GptPrivacyPolicy(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.GptPrivacyPolicy, w, r)
}


func GptBeMyPrivacyAgent(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.GptBeMyPrivacyAgent, w, r)
}
