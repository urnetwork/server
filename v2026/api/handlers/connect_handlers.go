package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

func ConnectControl(w http.ResponseWriter, r *http.Request) {
	r, finish := router.ObserveConnectControl(r)
	defer finish()
	router.WrapWithInputRequireClient(controller.ConnectControl, w, r)
}

// Bind the router-owned notifier without creating a request-owned subscription
// service or replacing the HTTP request's cancellation and deadline.
func ConnectControlWithOriginNotifications(notifications *model.ContractOriginNotifications) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ConnectControl(w, r.WithContext(model.WithContractOriginNotifications(r.Context(), notifications)))
	}
}

// GetClientKey backs `GET /key/<client_id>`. Unauthenticated by design: the
// value is a public Ed25519 key meant to be fetchable by any peer that wants to
// bind a client_id to a signing key out-of-band of the contract pipeline.
func GetClientKey(w http.ResponseWriter, r *http.Request) {
	pathValues := router.GetPathValues(r)
	clientId, err := server.ParseId(pathValues[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	args := &controller.GetClientKeyArgs{
		ClientId: clientId,
	}
	impl := func(clientSession *session.ClientSession) (*controller.GetClientKeyResult, error) {
		return controller.GetClientKey(args, clientSession)
	}
	router.WrapNoAuth(impl, w, r)
}

// GetClientKeyHistory backs `GET /key/<client_id>/history`. Unauthenticated for
// the same reason as GetClientKey: it returns already-published signed
// evidence. An operator that does not run the signed path returns an empty
// history with 200, so a client can tell "no signed evidence" from "could not
// reach the operator" without parsing status codes.
func GetClientKeyHistory(w http.ResponseWriter, r *http.Request) {
	pathValues := router.GetPathValues(r)
	clientId, err := server.ParseId(pathValues[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	args := &controller.GetClientKeyHistoryArgs{
		ClientId: clientId,
	}
	impl := func(clientSession *session.ClientSession) (*controller.GetClientKeyHistoryResult, error) {
		return controller.GetClientKeyHistory(args, clientSession)
	}
	router.WrapNoAuth(impl, w, r)
}
