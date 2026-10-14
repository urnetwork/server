package session

import (
	"net/http"
	"strings"
)

// No cookies (AUTH.md §1).
//
// No server reads a cookie, sets one or keeps state in one. Every credential
// travels in the Authorization header (RFC 6750), set by the caller on each
// request, so a request carries only what its sender chose to send and the
// browser attaches no credential of its own. A browser's login lives in
// storage on the ur.io origin (IDP.md §1), not in a cookie here.
//
// The rule is also what keeps the api's domain safe to share. Any host under
// bringyour.com can set a cookie for the whole domain and receives every
// cookie scoped to it, and hosts the operator does not run may be named under
// it (the planned extender names). A cookie scoped to the domain would reach
// each of them, and one they set would reach the api.
//
// So cookie headers are dropped, not merely left unread. The router drops a
// request's cookie headers before any route runs, and
// NewClientSessionFromRequest drops them again, so neither a handler nor
// ClientSession.Header sees one. The router's response writer drops the ones a
// handler adds before the header is sent. A feature that seems to need a
// cookie needs a different design.

// The request headers that carry cookies: Cookie (RFC 6265) and the obsolete
// Cookie2 (RFC 2965).
var cookieRequestHeaders = []string{"Cookie", "Cookie2"}

// The response headers that set cookies: Set-Cookie (RFC 6265) and the
// obsolete Set-Cookie2 (RFC 2965).
var cookieResponseHeaders = []string{"Set-Cookie", "Set-Cookie2"}

// DropCookies removes every cookie header from a request. The header map is
// written only when one is present, so dropping again from a request whose
// cookies are already gone is a read, safe beside other readers.
func DropCookies(req *http.Request) {
	dropHeaders(req.Header, cookieRequestHeaders)
}

// DropSetCookies removes every cookie-setting header from a response header
// map. It must run before the map is sent.
func DropSetCookies(header http.Header) {
	dropHeaders(header, cookieResponseHeaders)
}

// Names match in any letter case. A key written into the map directly skips
// canonicalization, the native writer sends it as written, and a browser
// matches header names without regard to case.
func dropHeaders(header http.Header, names []string) {
	for key := range header {
		for _, name := range names {
			if len(key) == len(name) && strings.EqualFold(key, name) {
				delete(header, key)
				break
			}
		}
	}
}
