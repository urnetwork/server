package handlers

import (
	"net/http"
	"strconv"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// §7.11's audit routes (TEAM-M6-09), each the network credential's
// (route_authz.go). Each answers 503 until the binary links MG1 and MG6, and
// 402 with a QuotaRefusal outside the team tier.

// GET /network/admin-events?after=<seq>&limit=<n>: the team's admin events
// after a seq, to Auditors and above.
func GetNetworkAdminEvents(w http.ResponseWriter, r *http.Request) {
	args := &model.GetNetworkAdminEventsArgs{}
	query := r.URL.Query()
	if value := query.Get("after"); value != "" {
		after, err := strconv.ParseInt(value, 10, 64)
		if err != nil || after < 0 {
			http.Error(w, "Invalid seq.", http.StatusBadRequest)
			return
		}
		args.After = after
	}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit <= 0 {
			http.Error(w, "Invalid limit.", http.StatusBadRequest)
			return
		}
		args.Limit = limit
	}
	impl := func(clientSession *session.ClientSession) (*model.GetNetworkAdminEventsResult, error) {
		return model.GetNetworkAdminEventsForSession(args, clientSession)
	}
	router.WrapRequireAuth(impl, w, r)
}

// POST /network/audit-webhook: adds, removes or lists the team's SIEM
// webhooks, the Owner's by the role gate. An added webhook's signing key is
// answered once and never logged: the router logs no result, and the result
// prints as its type.
func SetNetworkAuditWebhook(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.SetNetworkAuditWebhook, w, r)
}
