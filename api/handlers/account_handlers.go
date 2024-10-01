package handlers

import (
	"net/http"

	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/router"
)

func FeedbackSend(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.FeedbackSend, w, r)
}

func GetNetworkReferralCode(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkReferralCode, w, r)
}
