package router

// Differential backward-compat audit for the f9baf63e router rewrite (linear
// regex scan -> path-segment trie). For every real registered route table (api,
// connect, mcp, proxy-internal, taskworker) plus an adversarial table of
// overlapping/edge patterns, this asserts the trie matcher returns the SAME
// (winning route id, 200/404/405 status) as the original linear scan
// (linearMatch, defined in route_trie_test.go) across a large path corpus.
//
// A disagreement here is a real regression: a request that previously dispatched
// to handler X (or 404/405'd) now does something else.

import (
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// realApiRoutes is copied verbatim from api.Routes() (api/api.go) as of this
// audit, including the 5 routes that benchRoutes omits (/verify, /verify/keys,
// /sn/wallet, /sn/pool/claim, /sn/epoch). This is the table older api clients
// hit, so it is the one that matters for the backward-compat report.
var realApiRoutes = [][2]string{
	{"GET", "/privacy.txt"},
	{"GET", "/terms.txt"},
	{"GET", "/vdp.txt"},
	{"GET", "/status"},
	{"GET", "/stats/last-90"},
	{"GET", "/stats/providers-overview-last-90"},
	{"GET", "/stats/providers"},
	{"POST", "/stats/provider-last-90"},
	{"POST", "/stats/leaderboard"},
	{"POST", "/auth/login"},
	{"POST", "/auth/login-with-password"},
	{"POST", "/auth/verify"},
	{"GET", "/auth/refresh"},
	{"POST", "/auth/verify-send"},
	{"POST", "/auth/password-reset"},
	{"POST", "/auth/password-set"},
	{"POST", "/auth/network-check"},
	{"POST", "/auth/network-create"},
	{"POST", "/auth/network-delete"},
	{"POST", "/auth/code-create"},
	{"POST", "/auth/code-login"},
	{"POST", "/auth/upgrade-guest"},
	{"POST", "/auth/upgrade-guest-existing"},
	{"POST", "/network/auth-client"},
	{"POST", "/network/remove-client"},
	{"GET", "/network/clients"},
	{"GET", "/network/provider-locations"},
	{"POST", "/network/find-provider-locations"},
	{"POST", "/network/find-locations"},
	{"POST", "/network/find-providers"},
	{"POST", "/network/find-providers2"},
	{"POST", "/network/create-provider-spec"},
	{"GET", "/network/user"},
	{"POST", "/network/user/update"},
	{"GET", "/network/ranking"},
	{"POST", "/network/ranking-visibility"},
	{"POST", "/network/block-location"},
	{"POST", "/network/unblock-location"},
	{"GET", "/network/blocked-locations"},
	{"GET", "/network/reliability"},
	{"POST", "/preferences/set-preferences"},
	{"GET", "/preferences"},
	{"POST", "/feedback/send-feedback"},
	{"POST", "/pay/stripe"},
	{"POST", "/pay/coinbase"},
	{"POST", "/pay/circle"},
	{"POST", "/pay/play"},
	{"POST", "/pay/solana"},
	{"POST", "/solana/payment-intent"},
	{"POST", "/stripe/payment-intent"},
	{"POST", "/stripe/customer-portal"},
	{"GET", "/wallet/balance"},
	{"POST", "/wallet/validate-address"},
	{"POST", "/wallet/circle-init"},
	{"POST", "/wallet/circle-transfer-out"},
	{"GET", "/subscription/balance"},
	{"POST", "/subscription/check-balance-code"},
	{"POST", "/subscription/redeem-balance-code"},
	{"POST", "/subscription/create-payment-id"},
	{"POST", "/device/add"},
	{"POST", "/device/create-share-code"},
	{"GET", "/device/share-code/([^/]+)/qr.png"},
	{"POST", "/device/share-status"},
	{"POST", "/device/confirm-share"},
	{"POST", "/device/create-adopt-code"},
	{"GET", "/device/adopt-code/([^/]+)/qr.png"},
	{"POST", "/device/adopt-status"},
	{"POST", "/device/confirm-adopt"},
	{"POST", "/device/remove-adopt-code"},
	{"GET", "/device/associations"},
	{"POST", "/device/remove-association"},
	{"POST", "/device/set-association-name"},
	{"POST", "/device/set-provide"},
	{"POST", "/connect/control"},
	{"GET", "/key/([^/]+)"},
	{"POST", "/verify"},
	{"GET", "/verify/keys"},
	{"POST", "/sn/wallet"},
	{"GET", "/sn/pool/claim"},
	{"GET", "/sn/epoch"},
	{"GET", "/hello"},
	{"POST", "/account/api-key"},
	{"POST", "/account/api-key/remove"},
	{"GET", "/account/api-keys"},
	{"POST", "/account/payout-wallet"},
	{"GET", "/account/payout-wallet"},
	{"POST", "/account/circle-wallet"},
	{"POST", "/account/wallet"},
	{"GET", "/account/wallets"},
	{"POST", "/account/wallets/remove"},
	{"POST", "/account/wallets/verify-seeker"},
	{"GET", "/account/payments"},
	{"GET", "/account/referral-code"},
	{"GET", "/account/referral-network"},
	{"GET", "/account/unlink-referral-network"},
	{"POST", "/account/set-referral"},
	{"GET", "/account/points"},
	{"GET", "/account/balance-codes"},
	{"POST", "/referral-code/validate"},
	{"GET", "/transfer/stats"},
	{"GET", "/connect"},
	{"POST", "/connect"},
	{"POST", "/apple/notification"},
	{"GET", "/my-ip-info"},
	{"POST", "/updates/brevo"},
	{"POST", "/log/([^/]+)/upload"},
}

// connectRoutes is cli/connect/main.go: the websocket connect endpoint clients
// hit at "/", plus the status probe.
var connectRoutes = [][2]string{
	{"GET", "/status"},
	{"GET", "/"},
}

// mcpRoutes is cli/mcp/server.go: a "*" method catch-all `.*` after /status.
var mcpRoutes = [][2]string{
	{"GET", "/status"},
	{"*", ".*"},
}

// proxyInternalRoutes is proxy/proxy_api.go.
var proxyInternalRoutes = [][2]string{
	{"POST", "/warmup"},
}

// statusOnlyRoutes covers cli/proxy/main.go and cli/taskworker/main.go.
var statusOnlyRoutes = [][2]string{
	{"GET", "/status"},
}

// adversarialRoutes deliberately packs overlapping patterns (static vs dynamic
// at several prefix depths, both declaration orders, cross-slash `(.*)`/`.*`
// tails, an optional-char segment, a char-class quantifier, alternation, a
// literal-dot-vs-wildcard collision, a root catch-all, a method="*" capture
// route overlapping a static, and duplicate method/path) to hunt for any trie
// precedence or consideration bug the current real tables happen not to expose.
var adversarialRoutes = [][2]string{
	{"GET", "/a/b/c"},
	{"GET", "/a/([^/]+)/c"},
	{"GET", "/a/b/([^/]+)"},
	{"POST", "/a/(.*)"},
	{"GET", "/a/(.*)"},
	{"GET", "/a/b"},
	{"GET", "/files/x/y"},
	{"GET", "/files/(.*)"},
	{"GET", "/files/(.*)/tail"},
	{"GET", "/x.y/z"},
	{"GET", "/xyz/z"},
	{"GET", "/(foo|bar)/z"},
	{"GET", "/opt/b?/c"},
	{"GET", "/num/[0-9]+/end"},
	{"*", "/star/([^/]+)"},
	{"GET", "/star/specific"},
	{"POST", "/a/b/c"},
	{"GET", ".*"},
	{"GET", "/"},
}

type namedTable struct {
	name  string
	table [][2]string
}

func allNamedTables() []namedTable {
	return []namedTable{
		{"api", realApiRoutes},
		{"connect", connectRoutes},
		{"mcp", mcpRoutes},
		{"proxyInternal", proxyInternalRoutes},
		{"statusOnly", statusOnlyRoutes},
		{"adversarial", adversarialRoutes},
	}
}

func buildRoutesTable(table [][2]string) []*Route {
	routes := make([]*Route, len(table))
	for i, mp := range table {
		routes[i] = NewRoute(mp[0], mp[1], noopHandler)
	}
	return routes
}

// canonicalPath turns a route pattern into a path that should match it, by
// substituting each dynamic token with a concrete value.
func canonicalPath(pattern string) string {
	p := pattern
	p = strings.ReplaceAll(p, "([^/]+)", "seg")
	p = strings.ReplaceAll(p, "(.*)", "m1/m2")
	p = strings.ReplaceAll(p, "(foo|bar)", "foo")
	p = strings.ReplaceAll(p, "[0-9]+", "42")
	p = strings.ReplaceAll(p, ".*", "m1/m2")
	p = strings.ReplaceAll(p, "b?", "b")
	return p
}

// mutations returns a family of near-miss variants of a path that probe the
// edges the audit cares about: trailing slash, extra/one-fewer segment, a
// doubled slash (leading and interior), uppercasing, and '.'->'X' to exercise
// the regex '.' wildcard vs a literal dot.
func mutations(path string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	add(path)
	add(path + "/")
	add(path + "/extra")
	if strings.HasPrefix(path, "/") {
		add("/" + path) // doubled leading slash
	}
	add(strings.ToUpper(path))
	if i := strings.LastIndexByte(path, '/'); i > 0 {
		add(path[:i]) // drop last segment
	}
	for i := 1; i < len(path); i++ {
		if path[i] == '/' {
			add(path[:i] + "/" + path[i:]) // double an interior slash
			break
		}
	}
	if strings.Contains(path, ".") {
		add(strings.ReplaceAll(path, ".", "X"))
	}
	return out
}

// extraProbes are structural edge paths always included for every table.
var extraProbes = []string{
	"", "/", "//", "///",
	"/a", "/a/", "/a/b", "/a/b/", "/a//b", "/a/b/c", "/a/b/c/", "/a/b/c/d",
	"/status", "/status/", "/STATUS", "/Status", "/status/x",
	"/opt/b/c", "/opt//c", "/opt/c", "/opt/bb/c",
	"/num/42/end", "/num/abc/end", "/num//end", "/num/42/43/end",
	"/files", "/files/", "/files/x", "/files/x/y", "/files/x/y/z", "/files/a/b/c",
	"/files/a/b/tail", "/files//tail", "/files/x/y/tail",
	"/x.y/z", "/xXy/z", "/x/y/z", "/xyz/z",
	"/foo/z", "/bar/z", "/baz/z", "/foo/z/",
	"/star/specific", "/star/other", "/star/", "/star/specific/x",
	"/key/abc", "/key/abc/", "/key/a/b", "/key", "/key/",
	"/device/share-code/xyz/qr.png", "/device/share-code/xyz/qrXpng",
	"/device/share-code/xyz/qr.png/extra", "/device/share-code//qr.png",
	"/device/share-code/xyz", "/device/adopt-code/xyz/qr.png",
	"/log/f/upload", "/log//upload", "/log/f/g/upload", "/log/f/upload/", "/log/f/upload/x",
	"/privacy.txt", "/privacyXtxt", "/privacy.txt/", "/terms.txt", "/vdp.txt",
	"/account/api-key", "/account/api-keys", "/account/api-key/remove", "/account/api-key/x",
	"/account/wallet", "/account/wallets", "/account/wallets/remove", "/account/payout-wallet",
	"/network/user", "/network/user/update", "/network//user", "/network/user/",
	"/connect", "/connect/", "/connect/control", "/a/b", "/a/x/c", "/a/b/x",
}

// corpusFor builds the deduped path corpus for a table: every pattern's
// canonical path (plus mutations) unioned with the shared structural probes.
func corpusFor(table [][2]string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, mp := range table {
		for _, m := range mutations(canonicalPath(mp[1])) {
			add(m)
		}
	}
	for _, p := range extraProbes {
		add(p)
	}
	sort.Strings(out)
	return out
}

var probeMethods = []string{"GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS", "PATCH"}

// reachable reports whether path is something a normal net/http origin-form
// request can actually deliver to ServeHTTP. Go's URL parser guarantees such a
// path is non-empty and begins with '/'; an empty path only arises from an
// absolute-form request-target (GET http://host) and a path with no leading
// slash is rejected as an invalid request URI before routing. Backward-compat
// only concerns reachable paths.
func reachable(path string) bool {
	return strings.HasPrefix(path, "/")
}

// TestTrieMatchesLinear_RealTables is the decisive differential audit: over the
// actual registered route tables of every service (plus an adversarial overlap
// table), the trie must return exactly the same (winning route id, status) as
// the original linear scan for every reachable request path. Any disagreement
// is a real regression (a previously-served request now 404s/405s or dispatches
// to a different handler).
func TestTrieMatchesLinear_RealTables(t *testing.T) {
	total, checks, skipped := 0, 0, 0
	for _, nt := range allNamedTables() {
		routes := buildRoutesTable(nt.table)
		trie := newRouteTrie(routes)
		for _, path := range corpusFor(nt.table) {
			if !reachable(path) {
				skipped++
				continue
			}
			for _, method := range probeMethods {
				checks++
				wantID, wantStatus := linearMatch(routes, method, path)
				gotID, gotStatus := trieMatchStatus(trie, method, path)
				if gotStatus != wantStatus || gotID != wantID {
					total++
					if total <= 40 {
						t.Errorf("[%s] REGRESSION %s %q: trie=(id=%q status=%d) linear=(id=%q status=%d)",
							nt.name, method, path, gotID, gotStatus, wantID, wantStatus)
					}
				}
			}
		}
	}
	if total == 0 {
		t.Logf("all clear: trie == linear scan on %d reachable (method,path) probes (%d malformed skipped) across %d tables",
			checks, skipped, len(allNamedTables()))
	} else {
		t.Errorf("found %d trie/linear disagreements on reachable paths", total)
	}
}

// TestTrieLeniency_MalformedPathsOnly pins down the single behavioral difference
// the audit found between the trie and the old linear scan. It is confined to
// MALFORMED paths (empty, or not starting with '/') that a real client cannot
// deliver, and it is strictly in the *lenient* direction: the trie may SERVE a
// route where the old scan returned 404/405, but never the reverse. The reverse
// (old scan served, trie does not) would be a genuine can't-connect regression,
// so this test fails loudly if it ever appears.
func TestTrieLeniency_MalformedPathsOnly(t *testing.T) {
	lenient := 0
	for _, nt := range allNamedTables() {
		routes := buildRoutesTable(nt.table)
		trie := newRouteTrie(routes)
		for _, path := range corpusFor(nt.table) {
			if reachable(path) {
				continue
			}
			for _, method := range probeMethods {
				wantID, wantStatus := linearMatch(routes, method, path)
				gotID, gotStatus := trieMatchStatus(trie, method, path)
				if gotStatus == wantStatus && gotID == wantID {
					continue
				}
				// The old scan served this malformed request -> trie must too,
				// or we have turned a working request into a failure.
				if wantStatus == http.StatusOK {
					t.Errorf("[%s] STRICT-DIRECTION regression on malformed %s %q: linear served id=%q, trie=(id=%q status=%d)",
						nt.name, method, path, wantID, gotID, gotStatus)
					continue
				}
				lenient++
				t.Logf("[%s] benign leniency: malformed %s %q trie=(id=%q status=%d) vs linear=(id=%q status=%d)",
					nt.name, method, path, gotID, gotStatus, wantID, wantStatus)
			}
		}
	}
	t.Logf("malformed-path divergences (all benign/lenient): %d", lenient)
}

// TestTrieMatchesLinear_Fuzz throws seeded random structural paths (built from a
// small alphabet including empty segments, dotted segments and the real dynamic
// slot values) at the api and adversarial tables to catch anything the
// structured corpus misses.
func TestTrieMatchesLinear_Fuzz(t *testing.T) {
	alphabet := []string{
		"", "a", "b", "c", "seg", "x", "y", "z", "status", "key", "connect",
		"device", "share-code", "qr.png", "qrXpng", "log", "upload", "account",
		"api-key", "api-keys", "wallets", "remove", "files", "tail", "star",
		"specific", "foo", "bar", "42", "network", "user", "update",
	}
	rng := rand.New(rand.NewSource(0x5eed1234))
	tables := []namedTable{
		{"api", realApiRoutes},
		{"adversarial", adversarialRoutes},
		{"mcp", mcpRoutes},
		{"connect", connectRoutes},
	}
	for _, nt := range tables {
		routes := buildRoutesTable(nt.table)
		trie := newRouteTrie(routes)
		mismatches := 0
		for iter := 0; iter < 60000; iter++ {
			depth := 1 + rng.Intn(5)
			var b strings.Builder
			for d := 0; d < depth; d++ {
				b.WriteByte('/')
				b.WriteString(alphabet[rng.Intn(len(alphabet))])
			}
			if rng.Intn(4) == 0 {
				b.WriteByte('/') // random trailing slash
			}
			path := b.String()
			method := probeMethods[rng.Intn(len(probeMethods))]
			wantID, wantStatus := linearMatch(routes, method, path)
			gotID, gotStatus := trieMatchStatus(trie, method, path)
			if gotStatus != wantStatus || gotID != wantID {
				mismatches++
				if mismatches <= 25 {
					t.Errorf("[%s fuzz] REGRESSION %s %q: trie=(id=%q status=%d) linear=(id=%q status=%d)",
						nt.name, method, path, gotID, gotStatus, wantID, wantStatus)
				}
			}
		}
		if mismatches == 0 {
			t.Logf("[%s fuzz] all clear over 60000 random paths", nt.name)
		}
	}
}
