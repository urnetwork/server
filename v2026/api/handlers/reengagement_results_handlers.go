package handlers

import (
	"net/http"
	"strconv"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

// AdminReengagementResults pages the nightly re-engagement results aggregate
// (mmm/reengagement/PLAN.md "MEASUREMENT"). Auth is the onboarding admin
// bearer, checked by the controller (401 / 403), as AdminOnboardingResults.
func AdminReengagementResults(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := 0
	if v := query.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	args := &controller.AdminReengagementResultsArgs{
		Experiment: query.Get("experiment"),
		From:       query.Get("from"),
		To:         query.Get("to"),
		Stream:     query.Get("stream"),
		Platform:   query.Get("platform"),
		Tier:       query.Get("tier"),
		Cursor:     query.Get("cursor"),
		Limit:      limit,
	}
	router.WrapNoAuth(
		func(clientSession *session.ClientSession) (*controller.AdminReengagementResultsResult, error) {
			return controller.AdminReengagementResults(args, clientSession)
		},
		w,
		r,
	)
}
