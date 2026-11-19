package nginx

// The headers the lb's locations add to responses (owner: s-http), as the
// nginx generator writes them (warp/warpctl/config.go addServiceBlocks,
// addFrameOptions) and nginx's headers filter applies them
// (ngx_http_headers_filter_module):
//
//   - a service name's locations add the security headers and, for a
//     service with cors_origins, the CORS headers, all `always`, so on every
//     status, upstream errors and the front's own 413, 429 and 502 included;
//   - the /status deny, the lb names, port 80 and the redirects add none;
//   - the lb status routes add Content-Type: application/json, not `always`.
//
// nginx appends each added header to what the response already has: an
// upstream's own Vary or Access-Control-Allow-Origin stays beside the added
// one. The one change made here is Content-Type on the status routes, which
// replaces instead of being sent twice (LBALT-IMPL.md §4).

import (
	"net/http"
	"regexp"
	"strings"
)

// The values the generator writes, byte for byte, so the parity harness can
// compare them.
const (
	responseStrictTransportSecurity = "max-age=63072000; includeSubDomains; preload"
	responseFrameOptionsSameOrigin  = "SAMEORIGIN"
	responseContentTypeOptions      = "nosniff"
	responseXssProtection           = "1; mode=block"

	responseCorsAllowMethods   = "GET, POST, OPTIONS"
	responseCorsAllowHeaders   = "DNT,User-Agent,X-Requested-With,If-Modified-Since,Cache-Control,Content-Type,Range,X-Client-Version,X-UR-ClientInfo,X-UR-Intended-Network-Id,Authorization,Accept,Mcp-Session-Id,MCP-Protocol-Version,Last-Event-ID,X-UR-X402,PAYMENT-SIGNATURE,X-PAYMENT"
	responseCorsExposeHeaders  = "Content-Length,Content-Range,Accept,Mcp-Session-Id,MCP-Protocol-Version,Last-Event-ID,PAYMENT-REQUIRED,PAYMENT-RESPONSE,X-PAYMENT-RESPONSE"
	responseCorsVary           = "Origin"
	responseCorsWildcardOrigin = "*"
	// a preflight's cache lifetime, in seconds, as the generator writes it
	responseCorsMaxAge            = "1728000"
	responsePreflightContentType  = "text/plain; charset=utf-8"
	responseStatusJsonContentType = "application/json"
)

// The generator's $warp_x_frame_options map, in its order: an upstream CSP
// whose frame-ancestors lists only quoted keywords keeps SAMEORIGIN, and one
// whose frame-ancestors names a source sends no X-Frame-Options, so a
// browser without CSP Level 2 doesn't refuse framing the service allowed.
// Go's RE2 agrees with nginx's PCRE on whether these patterns match.
var (
	responseFrameAncestorsKeywordsRegexp = regexp.MustCompile(`(?i)(?:^|[;,])\s*frame-ancestors(?:\s+'[^'\s;,]*')*\s*(?:[;,]|$)`)
	responseFrameAncestorsSourceRegexp   = regexp.MustCompile(`(?i)(?:^|[;,])\s*frame-ancestors(?:\s+[^\s;,]+)*?\s+[^'\s;,]`)
)

// The X-Frame-Options a response gets from its own Content-Security-Policy
// headers, which nginx joins with ", ": SAMEORIGIN, or "" for none. A
// response with no CSP, the front's own included, keeps SAMEORIGIN.
func responseFrameOptions(header http.Header) string {
	contentSecurityPolicy := strings.Join(header.Values("Content-Security-Policy"), ", ")
	switch {
	case contentSecurityPolicy == "":
		return responseFrameOptionsSameOrigin
	case responseFrameAncestorsKeywordsRegexp.MatchString(contentSecurityPolicy):
		return responseFrameOptionsSameOrigin
	case responseFrameAncestorsSourceRegexp.MatchString(contentSecurityPolicy):
		return ""
	default:
		return responseFrameOptionsSameOrigin
	}
}

// One service's CORS policy, from its resolved cors_origins. Read-only once
// built.
type responseCors struct {
	// "*" is listed: every response allows any origin, and has no Vary
	wildcard bool
	// otherwise the origins echoed back, matched exactly, case included
	origins map[string]bool
}

// The policy of a service's cors_origins, or nil when it lists none.
func newResponseCors(corsOrigins []string) *responseCors {
	if len(corsOrigins) == 0 {
		return nil
	}
	cors := &responseCors{
		wildcard: false,
		origins:  map[string]bool{},
	}
	for _, corsOrigin := range corsOrigins {
		if corsOrigin == responseCorsWildcardOrigin {
			cors.wildcard = true
		}
		cors.origins[corsOrigin] = true
	}
	return cors
}

// nginx's $cors_origin for a request: "*" in wildcard mode, else the
// request's Origin when it equals a listed origin exactly, else "". Repeated
// Origin headers are joined with ", " first, as nginx's $http_origin joins
// them, so they match nothing.
func (self *responseCors) allowOrigin(r *http.Request) string {
	if self.wildcard {
		return responseCorsWildcardOrigin
	}
	origin := strings.Join(r.Header.Values("Origin"), ", ")
	if self.origins[origin] {
		return origin
	}
	return ""
}

// What a location adds to its responses.
type responseHeaders struct {
	// the security headers, always: a service name's locations
	security bool
	// the CORS headers, always; nil for none
	cors *responseCors
	// Access-Control-Allow-Origin's value, set once the rewrite phase has
	// run. Empty sends none, as an add_header with an empty value adds
	// nothing, so a 413 answered before the rewrite phase has none.
	corsAllowOrigin string
	// Content-Type: application/json, not always: the lb status routes
	jsonContentType bool
	// the preflight's Access-Control-Max-Age, Content-Type and
	// Content-Length, not always
	preflight bool
}

// Adds the location's headers to a response of the status about to be
// written.
func (self *responseHeaders) apply(header http.Header, statusCode int) {
	// an add_header without `always` applies only to these statuses
	addsNotAlways := false
	switch statusCode {
	case http.StatusOK,
		http.StatusCreated,
		http.StatusNoContent,
		http.StatusPartialContent,
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusNotModified,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect:
		addsNotAlways = true
	}

	if self.security {
		header.Add("Strict-Transport-Security", responseStrictTransportSecurity)
		if frameOptions := responseFrameOptions(header); frameOptions != "" {
			header.Add("X-Frame-Options", frameOptions)
		}
		header.Add("X-Content-Type-Options", responseContentTypeOptions)
		header.Add("X-XSS-Protection", responseXssProtection)
	}
	if self.cors != nil {
		if self.corsAllowOrigin != "" {
			header.Add("Access-Control-Allow-Origin", self.corsAllowOrigin)
		}
		header.Add("Access-Control-Allow-Methods", responseCorsAllowMethods)
		header.Add("Access-Control-Allow-Headers", responseCorsAllowHeaders)
		header.Add("Access-Control-Expose-Headers", responseCorsExposeHeaders)
		if !self.cors.wildcard {
			header.Add("Vary", responseCorsVary)
		}
	}
	if addsNotAlways && self.preflight {
		header.Set("Access-Control-Max-Age", responseCorsMaxAge)
		header.Set("Content-Type", responsePreflightContentType)
		header.Set("Content-Length", "0")
	}
	if addsNotAlways && self.jsonContentType {
		header.Set("Content-Type", responseStatusJsonContentType)
	}
}
