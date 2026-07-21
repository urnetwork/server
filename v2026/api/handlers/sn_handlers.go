package handlers

import (
	"net/http"
	"strconv"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

// SnSetWallet backs `POST /sn/wallet` (sn/PLAN.md §5): sets the caller
// network's subnet claim coldkey. Network JWT auth; the ss58 format is
// validated in the controller.
func SnSetWallet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SnSetWallet, w, r)
}

// SnPoolClaim backs `GET /sn/pool/claim?epoch=N` (sn/PLAN.md §5): the
// caller network's merkle pool-payout claim. `epoch` is optional and
// defaults to the latest finalized epoch (epoch 0 is a real epoch, so only
// absence defaults). Network JWT auth — a provider fetches its own claim.
func SnPoolClaim(w http.ResponseWriter, r *http.Request) {
	poolClaim := &controller.SnPoolClaimArgs{}
	if epochStr := r.URL.Query().Get("epoch"); epochStr != "" {
		epoch, err := strconv.ParseUint(epochStr, 10, 64)
		if err != nil {
			http.Error(w, "Bad epoch.", http.StatusBadRequest)
			return
		}
		poolClaim.Epoch = &epoch
	}
	impl := func(clientSession *session.ClientSession) (*controller.SnPoolClaimResult, error) {
		return controller.SnPoolClaim(poolClaim, clientSession)
	}
	router.WrapRequireAuth(impl, w, r)
}

// SnEpoch backs `GET /sn/epoch`: the contract epoch clock mirrored from
// chain so clients do not need their own RPC. No auth by design (matching
// the connect binding) — the state is global chain-clock state with nothing
// per-caller in it.
func SnEpoch(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.SnEpoch, w, r)
}
