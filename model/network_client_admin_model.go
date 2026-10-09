package model

import (
	"context"

	"github.com/urnetwork/server"
)

// A client token (a by_jwt with a client_id) acts for its own client and the
// clients it created, and never administers the network or the account
// (AUTHZ1.md). The router refuses a client token on the admin routes; the
// model limits the routes a client token keeps to its own clients.

// NetworkRefusesClientAdmin reports whether the network's client tokens are
// refused on every admin route, including the ones the URnetwork apps call
// with their client token today. An Embed network hands its client tokens to
// a third party's users, so none of them may administer it: the network is
// Embed-enabled, or it has the Embed plan's client allowance.
func NetworkRefusesClientAdmin(ctx context.Context, networkId server.Id) bool {
	if NetworkEmbedEnabled(ctx, networkId) {
		return true
	}
	_, embedPlan := networkClientLimitOverride(ctx, networkId)
	return embedPlan
}
