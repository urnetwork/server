package handlers

import (
	"net/http"
	"strconv"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// GET /network/usage (DESIGN.md 20.14; QUOTA-M2-12): the caller's network's QuotaLimits, its stored count,
// storage_over and spent, and its day rows by meter for up to 13 months, newest first, a page at a time:
// ?count= rows (at most model's 1,000), after the cursor ?after_day=YYYY-MM-DD&after_meter= that the last
// page's next named. Network class with the read permission (route_authz.go, route_permission.go), and
// not on §7.4's sidecar allowlist.
func GetNetworkUsage(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	var after *model.NetworkUsageCursor
	if day := query.Get("after_day"); day != "" {
		after = &model.NetworkUsageCursor{UsageDay: day, Meter: query.Get("after_meter")}
	}
	dayCount := 0
	if value := query.Get("count"); value != "" {
		count, err := strconv.Atoi(value)
		if err != nil || count <= 0 {
			http.Error(w, "Invalid count.", http.StatusBadRequest)
			return
		}
		dayCount = count
	}
	impl := func(clientSession *session.ClientSession) (*model.NetworkUsage, error) {
		return model.GetNetworkUsage(clientSession.Ctx, clientSession.ByJwt.NetworkId, after, dayCount, server.NowUtc())
	}
	router.WrapRequireAuth(router.PrivateErrors(impl), w, r)
}
