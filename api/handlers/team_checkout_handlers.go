package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

// POST /network/team-checkout: a Stripe Checkout session that buys the team
// tier for the caller's team network (§20.5's Becoming Team; QUOTA-M6-02), the
// network credential's, and the Owner's or a Billing admin's by the role gate
// (route_permission.go). Answers 503 until the binary links MG6.
func TeamCheckout(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.TeamCheckout, w, r)
}
