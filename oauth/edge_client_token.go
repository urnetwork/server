package oauth

// The service token api's internal edge client route answers beside each urnetwork-edge platform client's jwt
// (QD-1054): sockets takes service tokens of audience sockets alone (QD-1029), and an edge client cannot use the
// token exchange, which takes a proof by a bound ClientKey, names the client's sidecar binding and grants from
// the network's verified projection, none of which an edge client or urnetwork-edge has. tlsedge's and mxedge's
// router's Finds and smtpedge's ur/smtpedge registrations present it.
//
// It names the client in urnetwork-edge by network_id and ur_client_id, with no sidecar, no member, no scope and
// no act, and is signed under the svc- set as the exchange signs, for a service token's 15 minutes. sockets takes
// a token of urnetwork-edge's with no sidecar as an edge caller's (socketsservice's authenticate), and refuses a
// removed edge client's by its client rev/ object, which model's edge client removal effect owes.
//
// The route mints one only for a client model.MintEdgeClient has just answered as active in urnetwork-edge, on
// the edge class's bearer and within edge.yml's block, host, slot and index, and those checks bound what a
// stolen edge bearer can mint: the environment's fixed edge clients. Like every service token it is never
// stored (§2.8 api-11).

import (
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
)

// The audience an edge client's token names: sockets', the one service an edge client calls.
const EdgeClientSocketsTokenAudience = "sockets"

// A service token of audience sockets naming clientId in urnetwork-edge's networkId, and when it ends, as its exp
// carries it, in whole seconds. An issuer with no svc- key mints nothing.
func MintEdgeClientSocketsToken(networkId server.Id, clientId server.Id) (string, time.Time, error) {
	token, expiry, err := MintServiceToken(&MintServiceTokenArgs{
		NetworkId: networkId,
		ClientId:  clientId,
		Audience:  EdgeClientSocketsTokenAudience,
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return token, gojwt.NewNumericDate(expiry).Time, nil
}
