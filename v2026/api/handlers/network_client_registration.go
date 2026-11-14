package handlers

// Explicit endpoint negotiation prevents an older server from ignoring a
// request identifier and allocating another client through the legacy route.

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
)

// Server authentication precedes allocation, and the finite body has no proxy
// or inline payment operation that could escape the registration transaction.
func RegisterNetworkClient(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	router.WrapWithInputRequireAuth(controller.RegisterNetworkClient, w, r)
}
