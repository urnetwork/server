package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

// Backs `POST /lag/drift-test` (lag.dev/DRIFTTEST.md P2): allocates a drift
// test for a visitor's address and answers its token and embed url.
//
// An Embed API: the network's root token or an API key, on a network with
// Embed enabled. Every other network is refused before any other work, and a
// client token by the route's class (route_authz.go).
func LagDriftTestAllocate(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.LagDriftTestAllocate, w, r)
}

// Backs `GET /lag/drift-test?drift_test_id=…` (P4): a drift test's runs and
// its verdict, for the network that allocated it, with the same credential.
func LagDriftTestGet(w http.ResponseWriter, r *http.Request) {
	args := &controller.LagDriftTestGetArgs{
		DriftTestId: r.URL.Query().Get("drift_test_id"),
	}
	router.WrapRequireAuth(func(clientSession *session.ClientSession) (*controller.LagDriftTestView, error) {
		return controller.LagDriftTestGet(args, clientSession)
	}, w, r)
}

// Backs `POST /lag/drift-test/start` (P5, W8): starts a token's next run, or
// a demo run, and answers its tickets.
//
// No credential but the token, which is in the body so that no proxy or log
// line carries it (P1).
func LagDriftTestStart(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.LagDriftTestStart, w, r)
}

// Backs `POST /lag/drift-test/finish` (P5): the frame closed a run's
// sessions. The token is the credential.
func LagDriftTestFinish(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.LagDriftTestFinish, w, r)
}

// Backs `POST /lag/drift-test/result` (P5): what the widget shows. The token
// is the credential.
func LagDriftTestResult(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.LagDriftTestResultRead, w, r)
}
