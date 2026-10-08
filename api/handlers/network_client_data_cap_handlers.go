package handlers

import (
	"net/http"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Per-client data caps for embedded clients (EMBED1.md). Invalid queries
// answer 200 with `error.message`, like a refused request.

func NetworkClientDataCapSet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.SetClientDataCap, w, r)
}

func NetworkClientDataCapGet(w http.ResponseWriter, r *http.Request) {
	args := &model.GetClientDataCapArgs{
		ClientId: r.URL.Query().Get("client_id"),
	}
	router.WrapRequireAuth(func(clientSession *session.ClientSession) (*model.ClientDataCapResult, error) {
		return model.GetClientDataCap(args, clientSession)
	}, w, r)
}

func NetworkClientDataCapsList(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	args := &model.ListClientDataCapsArgs{
		Cursor: query.Get("cursor"),
		Limit:  query.Get("limit"),
	}
	router.WrapRequireAuth(func(clientSession *session.ClientSession) (*model.ListClientDataCapsResult, error) {
		return model.ListClientDataCaps(args, clientSession)
	}, w, r)
}
