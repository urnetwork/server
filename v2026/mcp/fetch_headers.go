package mcp

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/http/httpguts"
)

var fetchForbiddenRequestHeaders = map[string]bool{
	"Connection":          true,
	"Content-Length":      true,
	"Host":                true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

var fetchCrossOriginRequestHeaders = map[string]bool{
	"Accept":          true,
	"Accept-Language": true,
	"Range":           true,
	"User-Agent":      true,
}

// validateFetchRequestMetadata bounds caller-controlled request metadata and
// rejects transport headers that net/http or the proxy must own.
func validateFetchRequestMetadata(args FetchArgs) error {
	if fetchMaxRequestBodyBytes < len(args.Body) {
		return fmt.Errorf("the request body exceeds %d bytes", fetchMaxRequestBodyBytes)
	}
	if fetchMaxHeaders < len(args.Headers) {
		return fmt.Errorf("the request has more than %d headers", fetchMaxHeaders)
	}

	headerBytes := 0
	for name, value := range args.Headers {
		if !httpguts.ValidHeaderFieldName(name) {
			return fmt.Errorf("invalid request header name")
		}
		if !httpguts.ValidHeaderFieldValue(value) {
			return fmt.Errorf("invalid request header value for %q", name)
		}

		canonicalName := http.CanonicalHeaderKey(name)
		if fetchForbiddenRequestHeaders[canonicalName] || strings.HasPrefix(canonicalName, "Proxy-") {
			return fmt.Errorf("request header %q is not allowed", name)
		}

		headerBytes += len(name) + len(value) + 4
		if fetchMaxHeaderBytes < headerBytes {
			return fmt.Errorf("request headers exceed %d bytes", fetchMaxHeaderBytes)
		}
	}

	return nil
}

// scopedFetchHeaders sends arbitrary caller headers only to the origin the
// caller selected. Referenced resources and cross-origin redirects receive a
// small non-credential allowlist.
func scopedFetchHeaders(credentialOrigin *url.URL, fetchUrl *url.URL, headers map[string]string) http.Header {
	scopedHeaders := http.Header{}
	sameOrigin := sameFetchOrigin(credentialOrigin, fetchUrl)
	for name, value := range headers {
		canonicalName := http.CanonicalHeaderKey(name)
		if sameOrigin || fetchCrossOriginRequestHeaders[canonicalName] {
			scopedHeaders.Set(canonicalName, value)
		}
	}
	return scopedHeaders
}

func stripCrossOriginRequestHeaders(request *http.Request) {
	for name := range request.Header {
		if !fetchCrossOriginRequestHeaders[http.CanonicalHeaderKey(name)] {
			request.Header.Del(name)
		}
	}
}

// originScopedCookieJar prevents a page from causing authenticated loads of a
// different origin. Responses may set cookies during a redirect, but cookies
// are sent only to the selected page origin and only that origin is persisted.
type originScopedCookieJar struct {
	jar        http.CookieJar
	sendOrigin *url.URL
}

func (self *originScopedCookieJar) Cookies(fetchUrl *url.URL) []*http.Cookie {
	if !sameFetchOrigin(self.sendOrigin, fetchUrl) {
		return nil
	}
	return self.jar.Cookies(fetchUrl)
}

func (self *originScopedCookieJar) SetCookies(fetchUrl *url.URL, cookies []*http.Cookie) {
	self.jar.SetCookies(fetchUrl, cookies)
}

func sameFetchOrigin(a *url.URL, b *url.URL) bool {
	return canonicalFetchOrigin(a) == canonicalFetchOrigin(b)
}

func canonicalFetchOrigin(fetchUrl *url.URL) string {
	if fetchUrl == nil {
		return ""
	}

	scheme := strings.ToLower(fetchUrl.Scheme)
	hostname := strings.ToLower(fetchUrl.Hostname())
	port := fetchUrl.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return scheme + "://" + net.JoinHostPort(hostname, port)
}
