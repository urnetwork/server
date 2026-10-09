package handlers

import (
	"net/http"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Per-client ACL groups (EMBED1.md). Refusals answer 200 with `error.message`.

func NetworkClientAclGroupSet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.SetNetworkClientAclGroup, w, r)
}

func NetworkClientAclGroupGet(w http.ResponseWriter, r *http.Request) {
	args := &model.GetNetworkClientAclGroupArgs{
		ClientId: r.URL.Query().Get("client_id"),
	}
	router.WrapRequireAuth(func(clientSession *session.ClientSession) (*model.NetworkClientAclGroupResult, error) {
		return model.GetNetworkClientAclGroup(args, clientSession)
	}, w, r)
}
