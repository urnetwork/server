package handlers

import (
	"net/http"
	"strconv"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// GET /sidecar/scim-changes?after=<change_id>&limit=<n> (QD-829; §7.6, D-TEAM-25): SCIM's queued removals of members
// outside the AdminSet, for the indexer ServiceSettings.scim_signer names to sign, a page at a time. Client class on
// §7.4's strict_exp allowlist (api/route_authz.go); the model answers only that indexer's current client token, 403
// to every other credential. A failure it does not name answers 503 with a fixed message.
func GetSidecarScimChanges(w http.ResponseWriter, r *http.Request) {
	args := &model.GetSidecarScimChangesArgs{}
	query := r.URL.Query()
	if value := query.Get("after"); value != "" {
		after, err := server.ParseId(value)
		if err != nil {
			http.Error(w, "Invalid change id.", http.StatusBadRequest)
			return
		}
		args.After = &after
	}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit <= 0 {
			http.Error(w, "Invalid limit.", http.StatusBadRequest)
			return
		}
		args.Limit = limit
	}
	impl := func(clientSession *session.ClientSession) (*model.GetNetworkScimChangesResult, error) {
		return model.GetSidecarScimChanges(args, clientSession)
	}
	router.WrapRequireAuth(router.PrivateErrors(impl), w, r)
}
