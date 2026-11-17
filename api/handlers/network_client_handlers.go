package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// AuthNetworkClient provisions a client, and is where an agent meets the plan's
// limits. For an agent that asks for terms it carries the full x402 round trip:
//
//	POST /network/auth-client  X-UR-X402: 1                              -> 402 + payment terms
//	POST /network/auth-client  X-UR-X402: 1, PAYMENT-SIGNATURE: <signed> -> 200, Pro active, client returned
//
// The 402 carries the x402 v2 terms in its PAYMENT-REQUIRED header and the v1
// terms as its body; a v1 client retries with X-PAYMENT instead. The retry
// carries the signed payment on the SAME request, which is settled before
// the auth runs -- so by the time AuthNetworkClient checks the limit, the network is
// already Pro and simply succeeds. Every caller that does not ask (the apps, the
// browser extensions, ur.io), and every caller while x402 is off, gets the normal
// JSON body with `upgrade_required` to prompt on instead
// (controller.X402QuotesClient).
func AuthNetworkClient(w http.ResponseWriter, r *http.Request) {
	if !controller.X402SettleInlineUpgrade(w, r) {
		// settlement failed and the response is already written
		return
	}

	router.WrapWithInputRequireAuth(
		controller.AuthNetworkClient,
		w,
		r,
		func(_ *session.ClientSession, result *model.AuthNetworkClientResult) bool {
			return controller.WriteX402UpgradeRequired(w, r, result)
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

// NetworkProxies lists the network's hosted proxy devices with their credentials;
// NetworkClients lists only the network's devices.
func NetworkProxies(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(model.GetNetworkProxies, w, r)
}

// NetworkProxyRotateExit moves a unique-exit proxy to a new unique exit
// (model/proxy_unique_exit_model.go).
func NetworkProxyRotateExit(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.RotateProxyExit, w, r)
}

func NetworkPeers(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(model.GetNetworkPeersForSession, w, r)
}
