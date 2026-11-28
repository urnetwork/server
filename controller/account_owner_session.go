package controller

import (
	"github.com/urnetwork/server/session"
)

// The session that may make the account changes only the network's own
// sign-in may make (an email change, a payout wallet disconnect): a network
// token of a person, never a client token, an API key (it acts as the network
// but is not its owner), an OAuth or MCP identity, or a guest. The model
// checks in the change's own transaction that the user still administers the
// network.
func accountOwnerSession(clientSession *session.ClientSession) bool {
	if clientSession == nil || clientSession.ByJwt == nil {
		return false
	}
	byJwt := clientSession.ByJwt
	return !clientSession.ApiKeyAuthenticated &&
		!clientSession.UnsignedIdentity &&
		byJwt.ClientId == nil &&
		!byJwt.GuestMode &&
		!byJwt.LagGuest
}
