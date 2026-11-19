package nginx

// One forwarded service's upstream blocks (owner: s-forward): a keepalive
// pool per block sized from the route table, nginx's weighted least_conn, or
// for a stateful service nginx's weighted Ketama ring on the client address,
// reproduced exactly (`<upstream host>:<port>` keys, 160 points per unit of
// weight) so a mixed rollout doesn't remap clients, and D7's retries: at most
// ForwardSettings.MaxUpstreamAttempts attempts on distinct eligible blocks,
// on a connect error, a timeout or an upstream 503, before any response
// reaches the client.

import (
	"net/http"
)

// A service's blocks and their pools. Safe for concurrent use.
type upstreamGroup struct {
}

var _ serviceForwarder = (*upstreamGroup)(nil)

// Builds the group for one service of a parsed table. Upstream targets are
// `<upstream.ipv4>:<block port>`. bodyStore holds buffered bodies for
// retries, and is shared by every service of the front.
func newUpstreamGroup(routeTable *RouteTable, service *RouteService, bodyStore *bodyStore, settings *FrontSettings) *upstreamGroup {
	panic("lbalt: not implemented")
}

// See serviceForwarder.
func (self *upstreamGroup) serveHttp(w http.ResponseWriter, r *http.Request, options forwardOptions) {
	panic("lbalt: not implemented")
}

// See serviceForwarder.
func (self *upstreamGroup) close() {
	panic("lbalt: not implemented")
}
