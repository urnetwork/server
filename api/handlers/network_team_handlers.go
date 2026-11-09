package handlers

import (
	"net/http"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// §7.5's team routes (TEAM-M6-01), each the network credential's
// (route_authz.go). Both answer 503 until the binary links MG6.

// POST /network/team-create: a new team network, its caller the first Owner.
func CreateNetworkTeam(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.CreateNetworkTeam, w, r)
}

// GET /network/members?after=<member_id>: the team's members, a page at a
// time. A network outside the team tier is refused with a QuotaRefusal (402).
func GetNetworkMembers(w http.ResponseWriter, r *http.Request) {
	args := &model.GetNetworkMembersArgs{}
	if value := r.URL.Query().Get("after"); value != "" {
		after, err := server.ParseId(value)
		if err != nil {
			http.Error(w, "Invalid member id.", http.StatusBadRequest)
			return
		}
		args.After = &after
	}
	impl := func(clientSession *session.ClientSession) (*model.GetNetworkMembersResult, error) {
		return model.GetNetworkMembers(args, clientSession)
	}
	router.WrapRequireAuth(impl, w, r)
}
