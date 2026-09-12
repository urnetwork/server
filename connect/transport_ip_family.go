package connect

// transport_ip_family.go — address-family intent and observation for one
// client connection (connect/IPV6.md A3).
//
// A family-pinned platform transport declares the family it intends to
// prove: the `X-UR-IpFamily` header on h1, or `Auth.ip_family` on the h1 v1
// auth frame and the h3 control stream. The server records that intent with
// the family it observed the connection arrive on, and the reliability
// aggregation counts the connection as proof of a family only when the two
// agree (model.ConnectionProvenIpFamily). A connection that declares no
// intent is legacy and counts as v4, which is what every provider was assumed
// to carry before the field existed. The hostnames a pinned transport dials
// (connect-v4, connect-v6) are a client-side steering device only: nothing
// here looks at the Host header.

import (
	"strconv"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

// ipFamilyIntentFromHeader parses the h1 intent header. "4" and "6" are the
// only intents; anything else, including absence, is legacy.
func ipFamilyIntentFromHeader(value string) int32 {
	intent, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	switch intent {
	case 4, 6:
		return int32(intent)
	default:
		return 0
	}
}

// authIpFamilyIntent is the normalized intent carried by an auth message:
// 0, 4 or 6. A nil auth is legacy.
func authIpFamilyIntent(auth *protocol.Auth) int {
	if auth == nil {
		return 0
	}
	switch auth.IpFamily {
	case 4, 6:
		return int(auth.IpFamily)
	default:
		return 0
	}
}

// connectionIpFamily returns the family the client address was observed on
// (4, 6, or 0 when it does not parse) and the normalized declared intent, and
// logs once at default level when the two disagree. The disagreement is
// evidence about the path (an extender or proxy in between, a misconfigured
// record), not a fault to act on here: the connection carries traffic as
// usual and simply proves nothing about the family it declared.
func connectionIpFamily(clientId server.Id, clientAddress string, auth *protocol.Auth) (ipVersion int, ipFamilyIntent int) {
	ipVersion = server.ClientAddressIpVersion(clientAddress)
	ipFamilyIntent = authIpFamilyIntent(auth)
	if ipFamilyIntent != 0 && ipFamilyIntent != ipVersion {
		glog.Infof(
			"[t]ip family intent %d observed %d: connection proves nothing for %s\n",
			ipFamilyIntent,
			ipVersion,
			clientId,
		)
	}
	return
}
