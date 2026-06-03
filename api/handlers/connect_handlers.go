package handlers

import (
	"net/http"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

func ConnectControl(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireClient(controller.ConnectControl, w, r)
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
