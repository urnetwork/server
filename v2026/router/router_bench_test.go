package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// benchRoutes mirrors the shape (count + patterns) of api.Routes() so the
// microbenchmarks reflect realistic routing cost.
var benchRoutes = [][2]string{
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

func noopHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func buildBenchRouter() *Router {
	routes := make([]*Route, len(benchRoutes))
	for i, mp := range benchRoutes {
		routes[i] = NewRoute(mp[0], mp[1], noopHandler)
	}
	return NewRouter(context.Background(), routes)
}

// benchmark the full ServeHTTP path for an early route, a late route, and a
// path-param (regex capture) route.
func benchServe(b *testing.B, method, path string) {
	router := buildBenchRouter()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(method, path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
	}
}

// BenchmarkServe_First hits an early route in the table.
func BenchmarkServe_First(b *testing.B) { benchServe(b, "GET", "/status") }

// BenchmarkServe_Last hits the final route in the table.
func BenchmarkServe_Last(b *testing.B) { benchServe(b, "POST", "/updates/brevo") }

// BenchmarkServe_PathParam hits a deep capture-group route.
func BenchmarkServe_PathParam(b *testing.B) { benchServe(b, "POST", "/log/abc123/upload") }

// BenchmarkMatch_Regex_Last measures the original approach: a linear scan
// running each route's regex until one matches. Kept as a baseline contrast to
// BenchmarkMatch_Trie_Last.
func BenchmarkMatch_Regex_Last(b *testing.B) {
	routes := make([]*Route, len(benchRoutes))
	for i, mp := range benchRoutes {
		routes[i] = NewRoute(mp[0], mp[1], noopHandler)
	}
	path := "/updates/brevo"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, route := range routes {
			matches := route.regex.FindStringSubmatch(path)
			if 0 < len(matches) {
				break
			}
		}
	}
}

// the real trie matcher, apples-to-apples with BenchmarkMatch_Regex_Last.
func BenchmarkMatch_Trie_Last(b *testing.B) {
	trie := newRouteTrie(buildRoutesFromTable())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = trie.match("POST", "/updates/brevo")
	}
}

func BenchmarkMatch_Trie_PathParam(b *testing.B) {
	trie := newRouteTrie(buildRoutesFromTable())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = trie.match("POST", "/log/abc123/upload")
	}
}

// cost of route.String() called per request in ServeHTTP for stats.
func BenchmarkRouteString(b *testing.B) {
	route := NewRoute("POST", "/updates/brevo", noopHandler)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = route.String()
	}
}

// discardWriter is a no-alloc ResponseWriter so we isolate ServeHTTP routing
// overhead from response recording.
type discardWriter struct{ h http.Header }

func (d *discardWriter) Header() http.Header         { return d.h }
func (d *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardWriter) WriteHeader(int)             {}

// BenchmarkServeOnly isolates the per-request routing overhead (trie match +
// stats, plus path-values context only for capture routes) by reusing one
// request and a discard writer.
func BenchmarkServeOnly_Last(b *testing.B) {
	router := buildBenchRouter()
	req := httptest.NewRequest("POST", "/updates/brevo", nil)
	w := &discardWriter{h: http.Header{}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		router.ServeHTTP(w, req)
	}
}

func BenchmarkServeOnly_First(b *testing.B) {
	router := buildBenchRouter()
	req := httptest.NewRequest("GET", "/status", nil)
	w := &discardWriter{h: http.Header{}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		router.ServeHTTP(w, req)
	}
}
