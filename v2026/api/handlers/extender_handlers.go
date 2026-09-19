package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
)

// ExtenderActivate backs `POST /network/extender-activate`
// (connect/EXTENDER.md C2).
//
// The client jwt is required because the activation is attributed to a network
// and a client and is rate limited per user. The route is served on the
// family-pinned api hosts, so the caller address the handler probes back has
// exactly one family.
func ExtenderActivate(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(controller.ExtenderActivate, w, r)
}

// ExtenderHint backs `GET /network/extender-hint` (connect/DESIGNNOTES4.md
// §4): the continent the operator places the caller's address on.
//
// No credential. The answer is derived from the caller's address, which the
// operator sees on every request anyway, and a client reads it before it has
// logged in.
func ExtenderHint(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.ExtenderHint, w, r)
}

// ExtenderLatencyReport backs `POST /network/extender-latency`
// (connect/DESIGNNOTES4.md §3): the provider attestations an extender
// forwards.
//
// The client jwt is required because every attestation is attributed to an
// extender the calling client activated, and the report is rate limited per
// user.
func ExtenderLatencyReport(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(controller.ExtenderLatencyReport, w, r)
}
