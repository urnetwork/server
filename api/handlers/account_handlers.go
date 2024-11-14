package handlers

import (
	"net/http"

	"github.com/urnetwork/server/bringyour/controller"
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/router"
)

func FeedbackSend(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.FeedbackSend, w, r)
}

func GetNetworkReferralCode(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkReferralCode, w, r)
}
