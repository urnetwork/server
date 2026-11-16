package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
)

// TestBalanceDrain makes the caller's own available balance read as zero for a
// bounded window. Only networks in the vault allowlist, or whose sign-in email
// is on a tests.yml bypass domain, may use it; see
// model/test_balance_drain_model.go.
func TestBalanceDrain(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.TestBalanceDrain, w, r)
}

// TestBalanceRestore ends the caller's own drain.
func TestBalanceRestore(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.TestBalanceRestore, w, r)
}
