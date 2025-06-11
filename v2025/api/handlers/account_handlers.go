package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/router"
)

func FeedbackSend(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.FeedbackSend, w, r)
}

func GetNetworkReferralCode(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkReferralCode, w, r)
}

func ValidateReferralCode(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.ValidateReferralCode, w, r)
}

func GetAccountPoints(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetAccountPoints, w, r)
}

func SetNetworkReferral(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SetNetworkReferral, w, r)
}

func GetReferralNetwork(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetReferralNetwork, w, r)
}
