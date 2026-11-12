package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// POST /network/member-invite (§7.5, TEAM-M6-04).
func CreateNetworkMemberInvite(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.CreateNetworkMemberInvite, w, r)
}

// POST /network/member-invite-accept (§7.5, TEAM-M6-04).
func AcceptNetworkMemberInvite(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.AcceptNetworkMemberInvite, w, r)
}

// POST /auth/network-switch (§7.5, TEAM-M6-04).
func NetworkSwitch(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.NetworkSwitch, w, r)
}
