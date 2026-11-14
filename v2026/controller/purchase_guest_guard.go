package controller

// Every purchase entry refuses a legacy guest network (support inbox 7334).
//
// A legacy guest network has no login method, so nothing can sign back in to
// what is bought on it: the customer of 7334 bought a year on the guest
// network the app was in. Server dd1e72668 refused the Pro checkouts and
// payment intents (refuseGuestPurchase in subscription_controller.go); these
// entries still sold to a guest:
//
//   - POST /pay/crypto/intent for a data pack (only a plan was refused): the
//     data lands on the caller's network;
//   - POST /pay/gamma/intent for a data pack delivered to the account;
//   - x402 (POST /x402/purchase, the inline settle on /network/auth-client,
//     and the MCP fetch tool): an agent paying with the caller's network;
//   - the buy-data pages' purchases for a network named by the buyer (POST
//     /pay/data/checkout, /pay/data/solana-intent, /pay/data/crypto-intent):
//     the named network may be a guest network whatever the buyer's session.
//
// A data pack bought as a code is not refused: the code is redeemed into a
// network the buyer chooses later. A store-verified purchase and every webhook
// still credit a guest network, as before: the store already took the money.

import (
	"context"
	"net/http"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The named network's sign-in lookup, model.NetworkHasAnyAuthMethod.
// Replaceable only by hermetic tests. Production never mutates it.
var purchaseNetworkHasAnyAuthMethod = model.NetworkHasAnyAuthMethod

// The refusal of a purchase for a named network that has no sign-in.
const purchaseGuestNetworkMessage = "That network has no sign-in. Sign in to it in the app and add a sign-in before buying for it."

// Whether a purchase for a network the buyer named must be refused: it is a
// legacy guest network.
func refuseGuestDestination(ctx context.Context, networkId server.Id) bool {
	if purchaseNetworkHasAnyAuthMethod(ctx, networkId) {
		return false
	}
	if glog.V(1) {
		glog.Infof("[sub]refused a purchase for guest network %s\n", networkId)
	}
	return true
}

// The guest refusal of an x402 payment, for every entry that settles one,
// before anything is verified or settled: nil when the caller's network has a
// sign-in, else a 403 with the guest refusal's message and code. The agent is
// told to have the account add a sign-in; no money moves.
func X402RefuseGuest(clientSession *session.ClientSession) error {
	if !refuseGuestPurchase(clientSession) {
		return nil
	}
	return &X402PaymentError{
		Status:  http.StatusForbidden,
		Message: purchaseGuestSignInRequiredMessage,
		Reason:  PurchaseErrorCodeGuestSignInRequired,
	}
}
