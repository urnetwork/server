package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Backs `POST /network/extender-camouflage-report`
// (connect/EXTENDER-SPLICE-ROLLOUT.md): what a client's and an extender's tcp
// camouflage came to, which only increments counters.
//
// The client jwt is required because a report is rate limited per client and an
// extender's part is taken only from the client that activated it. The route
// reads the body itself, past the credential and within its own bound, so every
// refusal is counted and answered the same way. The identity is not recorded.
func ExtenderCamouflageReport(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireClient(func(clientSession *session.ClientSession) (*controller.ExtenderCamouflageReportResult, error) {
		return controller.ExtenderCamouflageReport(
			http.MaxBytesReader(w, r.Body, controller.ExtenderCamouflageReportMaxByteCount),
			clientSession,
		)
	}, w, r)
}
