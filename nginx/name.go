package nginx

// Host normalization and name matching with nginx's semantics (owner: s-tls).
// The front matches the TLS SNI and each request's authority the same way:
// the SNI picks the certificate, and the authority picks the route (§4.2,
// D9).

// The host nginx's $host holds for an authority (ngx_http_validate_host):
// lowercased, its :port removed, and one trailing dot removed; an IPv6
// literal keeps its brackets. False, which nginx answers with a 400, for a
// host that is empty after that, holds "..", or holds a '/', a NUL, a space
// or another control character.
func normalizeHost(host string) (string, bool) {
	panic("lbalt: not implemented")
}

// What a served name routes to.
type nameMatch struct {
	// the owning service; nil for an lb name
	service *RouteService
	class   NameClass
	// the redirect's host, for NameClassRedirect
	redirectTarget string
}

// The route table's served names, indexed for matching. Read-only once built,
// so safe for concurrent use.
type nameMatcher struct {
}

// Indexes every served name of a parsed table: the lb names, every service's
// names and every redirect alias.
func newNameMatcher(routeTable *RouteTable) *nameMatcher {
	panic("lbalt: not implemented")
}

// Matches a normalized name as nginx's server_name does: an exact name first,
// then the longest leading wildcard that matches. A wildcard *.b.c matches
// one or more leading labels (a.b.c, x.a.b.c), never b.c itself. False for a
// name the table doesn't serve.
func (self *nameMatcher) match(name string) (nameMatch, bool) {
	panic("lbalt: not implemented")
}
