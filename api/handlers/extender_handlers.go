package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
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
