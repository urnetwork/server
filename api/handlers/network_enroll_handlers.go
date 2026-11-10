package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

// POST /network/enroll (§7.8, TEAM-M1-04): Public, its credential the auth key
// the body carries, limited per caller address and per key. The body is JSON,
// which the router logs by type alone, never its value, so the auth key never
// reaches a log (D-TEAM-22).
func NetworkEnroll(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.NetworkEnroll, w, r)
}
