package nginx

// Tests of the response side's pieces (response.go, response_headers.go):
// the X-Frame-Options map, CORS origin matching, nginx's Accept-Encoding
// reading and add_header's status rule. The handlers' tests cover them end
// to end.

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// lag.dev's Content-Security-Policy as its service sends it, with the given
// frame-ancestors, from warpctl's nginx_frame_options_test.go.
func testLagDevContentSecurityPolicy(frameAncestors string) string {
	return "default-src 'self'; connect-src 'self' https://api.service.example https://*.extender.service.example wss://*.extender.service.example; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self'; frame-ancestors " + frameAncestors + "; base-uri 'none'; form-action 'self'"
}

// The X-Frame-Options of warpctl's map for each of its test cases
// (frameOptionsCases): SAMEORIGIN unless the upstream's own CSP lets another
// origin frame the response, and SAMEORIGIN with no CSP at all.
func TestResponseFrameOptionsFollowTheUpstreamFrameAncestors(t *testing.T) {
	cases := []struct {
		name                    string
		contentSecurityPolicies []string
		frameOptions            string
	}{
		{name: "the widget, frame-ancestors https:", contentSecurityPolicies: []string{testLagDevContentSecurityPolicy("https:")}, frameOptions: ""},
		{name: "other responses, frame-ancestors 'none'", contentSecurityPolicies: []string{testLagDevContentSecurityPolicy("'none'")}, frameOptions: "SAMEORIGIN"},
		{name: "no CSP", frameOptions: "SAMEORIGIN"},
		{name: "frame-ancestors 'self'", contentSecurityPolicies: []string{testLagDevContentSecurityPolicy("'self'")}, frameOptions: "SAMEORIGIN"},
		{name: "a CSP without frame-ancestors", contentSecurityPolicies: []string{"default-src 'self'; img-src https:"}, frameOptions: "SAMEORIGIN"},
		{name: "frame-ancestors 'self' and another host", contentSecurityPolicies: []string{"default-src 'self'; frame-ancestors 'self' https://partner.example"}, frameOptions: ""},
		{name: "frame-ancestors in capitals", contentSecurityPolicies: []string{"FRAME-ANCESTORS HTTPS:"}, frameOptions: ""},
		{name: "frame-ancestors with no source", contentSecurityPolicies: []string{"default-src 'self'; frame-ancestors; base-uri 'none'"}, frameOptions: "SAMEORIGIN"},
		{name: "frame-ancestors 'unsafe-inline'", contentSecurityPolicies: []string{"frame-ancestors 'unsafe-inline'"}, frameOptions: "SAMEORIGIN"},
		{name: "a second policy with frame-ancestors 'none'", contentSecurityPolicies: []string{"frame-ancestors https:", "frame-ancestors 'none'"}, frameOptions: "SAMEORIGIN"},
		{name: "a second policy without frame-ancestors", contentSecurityPolicies: []string{"frame-ancestors https:", "default-src 'self'"}, frameOptions: ""},
	}
	for _, c := range cases {
		header := http.Header{}
		for _, contentSecurityPolicy := range c.contentSecurityPolicies {
			header.Add("Content-Security-Policy", contentSecurityPolicy)
		}
		if frameOptions := responseFrameOptions(header); frameOptions != c.frameOptions {
			t.Errorf("%s: X-Frame-Options %q, want %q", c.name, frameOptions, c.frameOptions)
		}
	}
}

// In echo mode only an Origin equal to a listed origin, case included, is
// echoed; repeated Origin headers match nothing; wildcard mode allows any.
func TestResponseCorsAllowOrigin(t *testing.T) {
	echo := newResponseCors([]string{"https://app.service.example", "https://other.service.example"})
	wildcard := newResponseCors([]string{"*"})
	if newResponseCors([]string{}) != nil {
		t.Fatal("no origins built a policy")
	}
	cases := []struct {
		cors        *responseCors
		origins     []string
		allowOrigin string
	}{
		{cors: echo, origins: []string{"https://app.service.example"}, allowOrigin: "https://app.service.example"},
		{cors: echo, origins: []string{"https://other.service.example"}, allowOrigin: "https://other.service.example"},
		{cors: echo, origins: []string{"https://APP.service.example"}, allowOrigin: ""},
		{cors: echo, origins: []string{"https://app.service.example/"}, allowOrigin: ""},
		{cors: echo, origins: []string{"https://evil.example"}, allowOrigin: ""},
		{cors: echo, origins: nil, allowOrigin: ""},
		{cors: echo, origins: []string{"https://app.service.example", "https://app.service.example"}, allowOrigin: ""},
		{cors: wildcard, origins: []string{"https://evil.example"}, allowOrigin: "*"},
		{cors: wildcard, origins: nil, allowOrigin: "*"},
	}
	for _, c := range cases {
		r := newTestRequest("GET", "api.service.example", "/", true)
		for _, origin := range c.origins {
			r.Header.Add("Origin", origin)
		}
		if allowOrigin := c.cors.allowOrigin(r); allowOrigin != c.allowOrigin {
			t.Errorf("origins %q (wildcard %t): %q, want %q", c.origins, c.cors.wildcard, allowOrigin, c.allowOrigin)
		}
	}
}

// nginx's ngx_http_gzip_accept_encoding: a gzip token in any case at a
// token start, ended by the value's end, a comma, or a q that isn't zero.
func TestResponseAcceptsGzipAsNginx(t *testing.T) {
	cases := []struct {
		acceptEncoding string
		acceptsGzip    bool
	}{
		{acceptEncoding: "gzip", acceptsGzip: true},
		{acceptEncoding: "gzip, deflate, br", acceptsGzip: true},
		{acceptEncoding: "gzip,deflate", acceptsGzip: true},
		{acceptEncoding: "deflate, gzip", acceptsGzip: true},
		{acceptEncoding: "br,gzip", acceptsGzip: true},
		{acceptEncoding: "GZIP", acceptsGzip: true},
		{acceptEncoding: "gzip;q=1.0", acceptsGzip: true},
		{acceptEncoding: "gzip;q=0.5, br", acceptsGzip: true},
		{acceptEncoding: "gzip ; q=0.001", acceptsGzip: true},
		{acceptEncoding: "gzip;q=0", acceptsGzip: false},
		{acceptEncoding: "gzip;q=0.000", acceptsGzip: false},
		{acceptEncoding: "gzip;q=", acceptsGzip: false},
		{acceptEncoding: "gzip;level=1", acceptsGzip: false},
		{acceptEncoding: "gzip;", acceptsGzip: true},
		{acceptEncoding: "x-gzip", acceptsGzip: false},
		{acceptEncoding: "gzipped", acceptsGzip: false},
		{acceptEncoding: "deflate", acceptsGzip: false},
		{acceptEncoding: "", acceptsGzip: false},
		{acceptEncoding: "gzi", acceptsGzip: false},
	}
	for _, c := range cases {
		if acceptsGzip := responseAcceptsGzip(c.acceptEncoding); acceptsGzip != c.acceptsGzip {
			t.Errorf("responseAcceptsGzip(%q) = %t, want %t", c.acceptEncoding, acceptsGzip, c.acceptsGzip)
		}
	}
}

// An always header is added on every status; a header without always only
// on nginx's list of statuses. Added headers are appended, never replacing
// the response's own, except the status routes' Content-Type, which
// replaces instead of being sent twice.
func TestResponseHeadersFollowAddHeaderStatusRules(t *testing.T) {
	headers := responseHeaders{
		security:        true,
		cors:            newResponseCors([]string{"https://app.service.example"}),
		corsAllowOrigin: "https://app.service.example",
		jsonContentType: true,
	}
	for _, statusCode := range []int{200, 201, 204, 206, 301, 302, 303, 304, 307, 308, 400, 403, 404, 413, 429, 500, 502, 503, 504} {
		header := http.Header{}
		header.Set("Vary", "Accept-Encoding")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Content-Type", "text/plain")
		headers.apply(header, statusCode)

		for name, want := range map[string]string{
			"Strict-Transport-Security":     "max-age=63072000; includeSubDomains; preload",
			"X-Content-Type-Options":        "nosniff",
			"X-XSS-Protection":              "1; mode=block",
			"Access-Control-Allow-Origin":   "https://app.service.example",
			"Access-Control-Allow-Methods":  "GET, POST, OPTIONS",
			"Access-Control-Allow-Headers":  responseCorsAllowHeaders,
			"Access-Control-Expose-Headers": responseCorsExposeHeaders,
		} {
			if values := header.Values(name); !slices.Equal(values, []string{want}) {
				t.Errorf("status %d: %s = %q, want %q", statusCode, name, values, want)
			}
		}
		if values := header.Values("Vary"); !slices.Equal(values, []string{"Accept-Encoding", "Origin"}) {
			t.Errorf("status %d: Vary = %q, want the upstream's and Origin", statusCode, values)
		}
		if values := header.Values("X-Frame-Options"); !slices.Equal(values, []string{"DENY", "SAMEORIGIN"}) {
			t.Errorf("status %d: X-Frame-Options = %q, want the upstream's and the added one", statusCode, values)
		}
		wantContentType := "text/plain"
		if statusCode < 400 {
			wantContentType = "application/json"
		}
		if values := header.Values("Content-Type"); !slices.Equal(values, []string{wantContentType}) {
			t.Errorf("status %d: Content-Type = %q, want %q", statusCode, values, wantContentType)
		}
	}

	wildcardHeader := http.Header{}
	(&responseHeaders{
		cors:            newResponseCors([]string{"*"}),
		corsAllowOrigin: "*",
	}).apply(wildcardHeader, http.StatusOK)
	if values := wildcardHeader.Values("Vary"); len(values) != 0 {
		t.Errorf("wildcard mode added Vary %q", values)
	}
	if strings.Join(wildcardHeader.Values("Access-Control-Allow-Origin"), ",") != "*" {
		t.Errorf("wildcard mode: Access-Control-Allow-Origin %q", wildcardHeader.Values("Access-Control-Allow-Origin"))
	}

	noOriginHeader := http.Header{}
	(&responseHeaders{
		cors: newResponseCors([]string{"https://app.service.example"}),
	}).apply(noOriginHeader, http.StatusOK)
	if _, ok := noOriginHeader["Access-Control-Allow-Origin"]; ok {
		t.Error("an empty $cors_origin added Access-Control-Allow-Origin")
	}
	if noOriginHeader.Get("Access-Control-Allow-Methods") == "" || noOriginHeader.Get("Vary") != "Origin" {
		t.Errorf("without an allowed origin the other CORS headers must stay: %v", noOriginHeader)
	}
}
