package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// AuthNetworkClient provisions a client, and is where an agent meets the plan's
// concurrent-client limit. It carries the full x402 round trip:
//
//	POST /network/auth-client                     -> 402 + payment terms
//	POST /network/auth-client  X-PAYMENT: <signed> -> 200, Pro active, client returned
//
// The retry carries the signed payment on the SAME request, which is settled before
// the auth runs -- so by the time AuthNetworkClient checks the limit, the network is
// already Pro and simply succeeds. When x402 is off, the limit still returns the
// normal JSON body with `upgrade_required` for human clients to prompt on.
func AuthNetworkClient(w http.ResponseWriter, r *http.Request) {
	if !controller.X402SettleInlineUpgrade(w, r) {
		// settlement failed and the response is already written
		return
	}

	router.WrapWithInputRequireAuth(
		model.AuthNetworkClient,
		w,
		r,
		func(result *model.AuthNetworkClientResult) bool {
			return controller.WriteX402UpgradeRequired(w, result)
		},
	)
}

func RemoveNetworkClient(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.RemoveNetworkClient, w, r)
}

func RemoveNetworkClients(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 100<<20) // 100 MB cap (allows ~2M UUIDs worst case)
	router.WrapWithInputRequireAuth(model.RemoveNetworkClients, w, r)
}

func RemoveNetwork(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.NetworkRemove, w, r)
}

func NetworkClients(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(model.GetNetworkClients, w, r)
}

func NetworkPeers(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(model.GetNetworkPeersForSession, w, r)
}
