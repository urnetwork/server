package egresshealth

import (
	"net/http"
)

// The browser request profile every request carries, and the headers never
// sent whatever supplies them.

// The shape every request this package makes carries: a
// browser's user agent and the headers that browser sends with a top-level
// navigation. It is data, not code, because it has to move with the browsers
// it imitates -- the server serves one with the destination pool
// (Pool.Profile), so refreshing it does not wait for a prober release.
//
// Why a browser at all, when this probe used to announce itself: the question
// it asks changed. It used to be only "does this tunnel carry bytes", which any
// client can ask. It is now also "do real sites serve this exit" (GEOMAP
// §11.3), and that is only answered by asking the way a real user asks.
// Measured on main, a handful of large sites refused the probe's old
// self-describing request for 95 % of healthy providers, which made the
// one-in-ten rule a verdict on the probe's request shape rather than on the
// exit (§10.5). The loads stay polite in the ways that matter to the sites:
// one small GET per site per run, the first kilobyte read and the connection
// closed, and a retry only minutes later.
//
// What this shapes, and what it does not. It sets the user agent and the
// headers a site sees. Accept-Encoding is deliberately not among them: Go's
// transport adds "gzip" itself when a request names no encoding, and only then
// decodes the body transparently -- setting the header here (a browser would
// say "gzip, deflate, br, zstd") would hand the capped read and every body
// check compressed bytes. The TLS fingerprint stays Go's, and so does the HTTP
// version (the provider tunnel's transport refuses h2 for the bandwidth
// probe's sake; see providertunnel). A bot manager that fingerprints the
// handshake still sees a Go client, which this profile does not address.
type RequestProfile struct {
	UserAgent string            `json:"user_agent"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// Returns the profile of Firefox 155 on 64-bit Windows making a top-level
// navigation typed into the address bar (Sec-Fetch-Site none, Sec-Fetch-User
// ?1): its user agent, Accept, Accept-Language, the Sec-Fetch family and
// Upgrade-Insecure-Requests, as that browser sends them.
//
// Firefox rather than the more common Chrome because a Chrome navigation is
// not complete without its user-agent client hints (sec-ch-ua and friends),
// whose brand list is regenerated on every major version; a Chrome agent
// without them, or with a stale list, is itself a tell. Firefox sends none, so
// this set is the whole of what it sends and stays self-consistent between
// refreshes. The server's copy (Pool.Profile) is the one that should track the
// current release; this default is the seed and the fallback.
//
// A function returning a fresh value rather than a shared var, for the reason
// Destinations copies the table: a caller that edited the map would change
// every later probe.
func DefaultRequestProfile() RequestProfile {
	return RequestProfile{
		UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:157.0) Gecko/20100101 Firefox/157.0",
		Headers: map[string]string{
			"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			"Accept-Language":           "en-US,en;q=0.5",
			"Upgrade-Insecure-Requests": "1",
			"Sec-Fetch-Dest":            "document",
			"Sec-Fetch-Mode":            "navigate",
			"Sec-Fetch-Site":            "none",
			"Sec-Fetch-User":            "?1",
		},
	}
}

// The profile a request actually carries. A profile with no user
// agent is treated as absent rather than sent: a request without one goes out
// as Go's "Go-http-client/1.1", which Wikimedia's robot policy refuses outright
// (measured: 403 under it, 200 under a real agent, same host and minute), so a
// server that served a half-filled profile would fail that destination for the
// whole fleet.
func (self RequestProfile) orDefault() RequestProfile {
	if self.UserAgent == "" {
		return DefaultRequestProfile()
	}
	return self
}

// Sets the profile on req, then the destination's own headers
// over it (cloudflare-dns.com answers 400 to the DoH JSON form without its
// Accept header, so a destination must be able to override the navigation's
// Accept), and drops whatever neverSent names from both.
func applyHeaders(req *http.Request, profile RequestProfile, destination map[string]string) {
	req.Header.Set("User-Agent", profile.UserAgent)
	for name, value := range profile.Headers {
		if !neverSent(name) {
			req.Header.Set(name, value)
		}
	}
	for name, value := range destination {
		if !neverSent(name) {
			req.Header.Set(name, value)
		}
	}
}

// Reports whether name is one of the headers dropped from every request,
// whatever supplied them. Range, because a byte-range request is one of the things bot managers
// refuse, and the capped read (Destination.maxBytes) already bounds what a
// load keeps. Accept-Encoding, because naming it by hand switches off the
// transport's transparent gzip decoding (see RequestProfile).
//
// The pool and its profile are data from the server, so the rule is enforced
// here, where every request is built, rather than trusted to the data.
func neverSent(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Range", "Accept-Encoding":
		return true
	default:
		return false
	}
}
