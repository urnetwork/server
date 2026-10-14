package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

// Backs `POST /lag/session` (lag.dev/PLAN.md A1): a lag.dev guest session.
//
// No credential: this is how a browser gets one. The body is `{}` and is not
// read, so an empty body serves too. Creation is limited per /24 (/48) of the
// caller.
func LagSession(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.LagSession, w, r)
}

// Backs `GET /lag/extenders` (A4): a sample of the extenders that serve the
// measurement api now.
//
// A client jwt is required, the lag guest's or any other client's, so the
// sample is drawn for a session rather than for anyone.
func LagExtenders(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireClient(controller.LagExtenders, w, r)
}

// Backs `POST /lag/track` (A6): every ticket of a track in one call.
//
// A client jwt is required because every ticket names the client it admits
// and tracks are limited per client.
func LagTrack(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(controller.LagTrack, w, r)
}

// Backs `POST /network/extender-lag-report` (A7): the lag.dev sessions an
// extender measured.
//
// The client jwt is the extender's activation credential, as for the ping
// report: every report must be signed by an extender the calling client
// activated.
func ExtenderLagReport(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(controller.ExtenderLagReport, w, r)
}
