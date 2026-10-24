package router

import (
	"net/http"
	"testing"
)

// linearMatch replicates the original ServeHTTP matching semantics (scan routes
// in order, first path+method match wins, else 405 if any path matched, else
// 404). The trie must agree with it on every input.
func linearMatch(routes []*Route, method string, path string) (id string, status int) {
	var allow []string
	for _, route := range routes {
		matches := route.regex.FindStringSubmatch(path)
		if 0 < len(matches) {
			if method != route.method && route.method != "*" {
				allow = append(allow, route.method)
				continue
			}
			return route.id, http.StatusOK
		}
	}
	if 0 < len(allow) {
		return "", http.StatusMethodNotAllowed
	}
	return "", http.StatusNotFound
}

func trieMatchStatus(trie *routeTrie, method string, path string) (id string, status int) {
	route, allow := trie.match(method, path)
	if route != nil {
		return route.id, http.StatusOK
	}
	if 0 < len(allow) {
		return "", http.StatusMethodNotAllowed
	}
	return "", http.StatusNotFound
}

func buildRoutesFromTable() []*Route {
	routes := make([]*Route, len(benchRoutes))
	for i, mp := range benchRoutes {
		routes[i] = NewRoute(mp[0], mp[1], noopHandler)
	}
	return routes
}

// TestTrieMatchesLinearScan asserts the trie reproduces the linear scan exactly
// across an adversarial path corpus over the real route-table shape.
func TestTrieMatchesLinearScan(t *testing.T) {
	routes := buildRoutesFromTable()
	trie := newRouteTrie(routes)

	type probe struct {
		method string
		path   string
	}
	probes := []probe{
		// exact static hits
		{"GET", "/status"},
		{"POST", "/auth/login"},
		{"POST", "/auth/login-with-password"},
		{"GET", "/network/user"},
		{"POST", "/network/user/update"},
		{"GET", "/account/wallets"},
		{"POST", "/account/wallets/remove"},
		{"POST", "/updates/brevo"},
		// both methods registered on the same path
		{"GET", "/connect"},
		{"POST", "/connect"},
		{"PUT", "/connect"}, // 405
		// static path, wrong method -> 405
		{"POST", "/status"},
		{"GET", "/auth/login"},
		{"DELETE", "/network/user"},
		// capture-group routes
		{"GET", "/key/abc123"},
		{"POST", "/key/abc123"}, // wrong method -> 405
		{"GET", "/device/share-code/xyz/qr.png"},
		{"GET", "/device/adopt-code/xyz/qr.png"},
		{"POST", "/log/file-1/upload"},
		{"GET", "/log/file-1/upload"}, // wrong method -> 405
		// capture edge cases (empty capture, missing tail, trailing slash)
		{"GET", "/key/"},
		{"GET", "/key"},
		{"POST", "/log//upload"},
		{"GET", "/device/share-code/xyz"},
		{"GET", "/device/share-code/xyz/qr.png/extra"},
		// the '.' in *.txt is a regex wildcard in the original; trie must agree
		{"GET", "/privacy.txt"},
		{"GET", "/privacyXtxt"},
		{"GET", "/terms.txt"},
		// prefixes / partials / trailing slashes -> 404
		{"GET", "/"},
		{"GET", "/auth"},
		{"GET", "/auth/"},
		{"GET", "/auth/login/"},
		{"GET", "/auth/login/extra"},
		{"GET", "/status/"},
		{"GET", "/network"},
		{"GET", "/nope"},
		{"GET", "/account/wallets/remove/x"},
		{"GET", "//"},
		{"GET", "/network//user"},
	}

	for _, p := range probes {
		wantId, wantStatus := linearMatch(routes, p.method, p.path)
		gotId, gotStatus := trieMatchStatus(trie, p.method, p.path)
		if gotStatus != wantStatus || gotId != wantId {
			t.Errorf("match(%s %s): trie=(%q,%d) linear=(%q,%d)",
				p.method, p.path, gotId, gotStatus, wantId, wantStatus)
		}
	}
}

// TestTriePrecedenceFollowsDeclarationOrder checks that when two routes overlap,
// the trie picks the lower-index route, matching the linear scan's first-wins.
func TestTriePrecedenceFollowsDeclarationOrder(t *testing.T) {
	cases := [][][2]string{
		// static before overlapping dynamic
		{{"GET", "/a/b/c"}, {"GET", "/a/([^/]+)/c"}},
		// dynamic before overlapping static
		{{"GET", "/a/([^/]+)/c"}, {"GET", "/a/b/c"}},
		// two overlapping dynamics at different prefix depths
		{{"GET", "/a/b/([^/]+)"}, {"GET", "/a/([^/]+)/c"}},
		{{"GET", "/a/([^/]+)/c"}, {"GET", "/a/b/([^/]+)"}},
	}
	probePaths := []string{"/a/b/c", "/a/x/c", "/a/b/x", "/a/b/c/d", "/a/b"}

	for ci, table := range cases {
		routes := make([]*Route, len(table))
		for i, mp := range table {
			routes[i] = NewRoute(mp[0], mp[1], noopHandler)
		}
		trie := newRouteTrie(routes)
		for _, path := range probePaths {
			wantId, wantStatus := linearMatch(routes, "GET", path)
			gotId, gotStatus := trieMatchStatus(trie, "GET", path)
			if gotStatus != wantStatus || gotId != wantId {
				t.Errorf("case %d GET %s: trie=(%q,%d) linear=(%q,%d)",
					ci, path, gotId, gotStatus, wantId, wantStatus)
			}
		}
	}
}

// TestStaticPrefixSegments spot-checks the prefix extraction, including the
// '/'-inside-a-char-class case and the literal-with-dot case.
func TestStaticPrefixSegments(t *testing.T) {
	type want struct {
		segs    []string
		dynamic bool
	}
	cases := map[string]want{
		"/auth/login":                       {[]string{"auth", "login"}, false},
		"/key/([^/]+)":                      {[]string{"key"}, true},
		"/device/share-code/([^/]+)/qr.png": {[]string{"device", "share-code"}, true},
		"/log/([^/]+)/upload":               {[]string{"log"}, true},
		"/privacy.txt":                      {nil, true}, // '.' is a metachar -> no literal prefix
		"/connect":                          {[]string{"connect"}, false},
	}
	for pattern, w := range cases {
		segs, dynamic := staticPrefixSegments(pattern)
		if dynamic != w.dynamic {
			t.Errorf("staticPrefixSegments(%q) dynamic=%v want %v", pattern, dynamic, w.dynamic)
		}
		if len(segs) != len(w.segs) {
			t.Errorf("staticPrefixSegments(%q) segs=%v want %v", pattern, segs, w.segs)
			continue
		}
		for i := range segs {
			if segs[i] != w.segs[i] {
				t.Errorf("staticPrefixSegments(%q) segs=%v want %v", pattern, segs, w.segs)
				break
			}
		}
	}
}
