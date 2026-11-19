// The connection side of a proxy egress. A client marks a platform transport
// it dialed through a proxy with the H1 `X-UR-Egress-Proxy: 1` header (connect
// sets it from the transport's ConnectSettings.ProxySettings, as an sn miner
// proxy slot dials). Such a transport connects from the proxy's address, not
// from its host's. Its egress lease still attributes it, but the client is
// marked first (model.FeedVerifyProxiedConnectionEgress), so it shows no head,
// capture the flag or extender unit (model/verify_proxied_egress_model.go).
//
// Only H1 carries the mark. A socks proxy carries no UDP, so connect offers no
// H3 through one, and an H3 transport is never a proxied one.
package connect

import (
	"net/http"
)

// the H1 connect handshake header that marks a transport dialed through a
// proxy
const HeaderEgressProxy = "X-UR-Egress-Proxy"

// Whether the handshake marks the transport as dialed through a proxy. Only
// the value 1 does.
func egressProxyFromHeader(header http.Header) bool {
	return header.Get(HeaderEgressProxy) == "1"
}
