package handlers

import (
	"net/http"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// GET /sidecar/limits (DESIGN.md 20.7, 20.14): the caller's network's QuotaLimits, its
// tier and every limit after its network_sidecar_limit rows, for local pre-checks.
// Client class on §7.4's strict_exp allowlist (api/route_authz.go). Reads no Postgres
// once the tier and the row snapshot are warm (R-QUOTA-5).
func GetSidecarLimits(w http.ResponseWriter, r *http.Request) {
	impl := func(clientSession *session.ClientSession) (*sidecar.QuotaLimits, error) {
		return model.NetworkQuotaLimits(clientSession.Ctx, clientSession.ByJwt.NetworkId), nil
	}
	router.WrapRequireAuth(router.PrivateErrors(impl), w, r)
}
