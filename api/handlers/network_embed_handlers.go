package handlers

import (
	"net/http"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// Per-network Embed state (EMBED1.md). Refusals answer 200 with
// `error.message`.

func NetworkEmbedGet(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(model.GetNetworkEmbedStatus, w, r)
}
