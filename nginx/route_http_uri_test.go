package nginx

// Tests of the request uri as nginx reads it (route_http_uri.go).

import (
	"testing"
)

// nginx's ngx_http_parse_complex_uri with merge_slashes on: escapes decoded
// as the path is scanned, so an escaped / or dot takes part in the path's
// syntax while an escaped %, # or ? is kept as a character; slashes merged;
// dot segments resolved; the query raw and cut at a fragment.
func TestParseRouteUriAsNginx(t *testing.T) {
	cases := []struct {
		requestTarget string
		path          string
		args          string
		ok            bool
	}{
		{requestTarget: "/", path: "/", ok: true},
		{requestTarget: "/status", path: "/status", ok: true},
		// the /status deny's own cases (behavior 29)
		{requestTarget: "//status", path: "/status", ok: true},
		{requestTarget: "/%73tatus", path: "/status", ok: true},
		{requestTarget: "/%73TATUS", path: "/sTATUS", ok: true},
		{requestTarget: "/status/", path: "/status/", ok: true},
		{requestTarget: "/STATUS", path: "/STATUS", ok: true},
		{requestTarget: "/a/../status", path: "/status", ok: true},
		{requestTarget: "/./status", path: "/status", ok: true},
		{requestTarget: "/a/%2e%2e/status", path: "/status", ok: true},
		{requestTarget: "/a%2fb", path: "/a/b", ok: true},
		{requestTarget: "/a%2F%2Fb", path: "/a/b", ok: true},
		// dot segments at the end
		{requestTarget: "/a/.", path: "/a/", ok: true},
		{requestTarget: "/a/..", path: "/", ok: true},
		{requestTarget: "/a/b/..", path: "/a/", ok: true},
		{requestTarget: "/a/..?x=1", path: "/", args: "x=1", ok: true},
		{requestTarget: "/a/.?x=1", path: "/a/", args: "x=1", ok: true},
		// a dot inside a segment is a character
		{requestTarget: "/a.b/c..d", path: "/a.b/c..d", ok: true},
		{requestTarget: "/.well-known/x", path: "/.well-known/x", ok: true},
		// escaped syntax that stays a character
		{requestTarget: "/a%3Fb?c", path: "/a?b", args: "c", ok: true},
		{requestTarget: "/a%23b", path: "/a#b", ok: true},
		{requestTarget: "/a%25b", path: "/a%b", ok: true},
		{requestTarget: "/a%20b", path: "/a b", ok: true},
		// the query
		{requestTarget: "/a?x=%2F&y", path: "/a", args: "x=%2F&y", ok: true},
		{requestTarget: "/a?x#frag", path: "/a", args: "x", ok: true},
		{requestTarget: "/a#frag?x", path: "/a", ok: true},
		{requestTarget: "/a?", path: "/a", ok: true},
		// refused, as nginx answers them with a 400
		{requestTarget: "/..", ok: false},
		{requestTarget: "/../a", ok: false},
		{requestTarget: "/a/../..", ok: false},
		{requestTarget: "/%2e%2e", ok: false},
		{requestTarget: "/a%00b", ok: false},
		{requestTarget: "/a%zzb", ok: false},
		{requestTarget: "/a%2", ok: false},
		{requestTarget: "/a%", ok: false},
		{requestTarget: "a/b", ok: false},
		{requestTarget: "*", ok: false},
		{requestTarget: "", ok: false},
	}
	for _, c := range cases {
		uri, ok := parseRouteUri(c.requestTarget)
		if ok != c.ok {
			t.Errorf("parseRouteUri(%q) ok = %t, want %t", c.requestTarget, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if uri.path != c.path || uri.args != c.args {
			t.Errorf("parseRouteUri(%q) = (%q, %q), want (%q, %q)", c.requestTarget, uri.path, uri.args, c.path, c.args)
		}
	}
}

// nginx's NGX_ESCAPE_URI escapes control bytes, space, #, %, ?, DEL and
// bytes over 0x7f in uppercase hex, and nothing else, so a decoded path goes
// upstream as nginx sends it.
func TestEscapeRouteUriPathAsNginx(t *testing.T) {
	cases := []struct {
		path        string
		escapedPath string
	}{
		{path: "/a/b", escapedPath: "/a/b"},
		{path: "/a b", escapedPath: "/a%20b"},
		{path: "/a#b?c%d", escapedPath: "/a%23b%3Fc%25d"},
		{path: "/caf\xc3\xa9", escapedPath: "/caf%C3%A9"},
		{path: "/\x01\x7f", escapedPath: "/%01%7F"},
		{path: "/a+b=c&d;e:f@g!h$i'j(k)l*m,n~o", escapedPath: "/a+b=c&d;e:f@g!h$i'j(k)l*m,n~o"},
	}
	for _, c := range cases {
		if escapedPath := escapeRouteUriPath(c.path); escapedPath != c.escapedPath {
			t.Errorf("escapeRouteUriPath(%q) = %q, want %q", c.path, escapedPath, c.escapedPath)
		}
	}

	uri, ok := parseRouteUri("/a%20b?q=%20")
	if !ok {
		t.Fatal("parseRouteUri refused a valid target")
	}
	if target := uri.upstreamTarget(uri.path); target != "/a%20b?q=%20" {
		t.Errorf("upstreamTarget = %q, want the path escaped and the args raw", target)
	}
	uri, _ = parseRouteUri("/a?")
	if target := uri.upstreamTarget(uri.path); target != "/a" {
		t.Errorf("upstreamTarget = %q, want no ? for empty args", target)
	}
}

// $request_uri is the target as sent in origin form, and the path and query
// of an absolute target; other forms are refused.
func TestRouteRequestUri(t *testing.T) {
	cases := []struct {
		requestTarget string
		requestUri    string
		ok            bool
	}{
		{requestTarget: "/a//b?c", requestUri: "/a//b?c", ok: true},
		{requestTarget: "http://api.service.example/a?b", requestUri: "/a?b", ok: true},
		{requestTarget: "https://api.service.example:443/a", requestUri: "/a", ok: true},
		{requestTarget: "http://api.service.example", requestUri: "/", ok: true},
		{requestTarget: "http://api.service.example?b", requestUri: "/?b", ok: true},
		{requestTarget: "*", ok: false},
		{requestTarget: "api.service.example:443", ok: false},
	}
	for _, c := range cases {
		requestUri, ok := routeRequestUri(c.requestTarget)
		if ok != c.ok || requestUri != c.requestUri {
			t.Errorf("routeRequestUri(%q) = (%q, %t), want (%q, %t)", c.requestTarget, requestUri, ok, c.requestUri, c.ok)
		}
	}
}
