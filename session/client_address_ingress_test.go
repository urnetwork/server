package session

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Regression suite for wrongful 429/503 on account creation, and for the HTTP
// 500 the first attempt at fixing it would have caused.
//
// Every per-address budget in the service keys off the address
// ResolveClientAddress returns: the account-creation limit (5 per 24h,
// model/network_create_rate_limit.go) and the auth-attempt limit (5 per 5min,
// model/auth_model_attempt.go). If the resolver hands back the ingress proxy's
// own address instead of the client's, all of those budgets collapse into ONE
// budget for the whole deployment, and legitimate first-time users are refused
// with "429 You have reached the maximum number of account creations for
// today." or "503 User auth attempts exceeded limits." for something five
// strangers did.
//
// The other half of this file is the opposite failure. The resolver used to
// require a header shape almost no real proxy sends -- X-Forwarded-For paired
// with X-Forwarded-Source-Port, single hop -- and returned an ERROR for
// anything else. router.wrap and router.wrapWithInput both turn a session
// construction failure into HTTP 500, on every endpoint. So an operator who
// read the "add its subnet to BRINGYOUR_TRUSTED_PROXY_CIDRS" report, enumerated
// their stock nginx, and stopped there took the entire api down: nginx sends a
// bare X-Forwarded-For, and $proxy_add_x_forwarded_for appends a comma on the
// second hop. Those shapes now resolve; anything still unreadable degrades to
// the peer address and is reported, never served as a 500.

const (
	// the ingress proxy's own address on a container bridge network
	ingressProxyAddress = "172.18.0.7:52344"
	ingressProxyCidr    = "172.16.0.0/12"
)

func ingressRequest(clientIp string, clientPort string) *http.Request {
	req := httptest.NewRequest("POST", "/auth/network-create", nil)
	req.RemoteAddr = ingressProxyAddress
	req.Header.Set("X-Forwarded-For", clientIp)
	req.Header.Set("X-Forwarded-Source-Port", clientPort)
	return req
}

// proxiedRequest is a request arriving from a trusted proxy carrying whatever
// header a real proxy would have set, and nothing this service invented.
func proxiedRequest(header string, value string) *http.Request {
	req := httptest.NewRequest("POST", "/auth/network-create", nil)
	req.RemoteAddr = ingressProxyAddress
	req.Header.Set(header, value)
	return req
}

func trustedIngress() []netip.Prefix {
	return []netip.Prefix{netip.MustParsePrefix(ingressProxyCidr)}
}

// TestResolveClientAddressSeparatesClientsBehindTrustedIngressProxy is the
// highest-value assertion in this suite: two unrelated signups arriving through
// the same proxy must not resolve to the same address. A resolver that returns
// the proxy address for both gives the entire fleet a single 5-per-24h signup
// budget.
func TestResolveClientAddressSeparatesClientsBehindTrustedIngressProxy(t *testing.T) {
	trusted := trustedIngress()

	first, err := ResolveClientAddress(ingressRequest("203.0.113.9", "41001"), trusted)
	if err != nil {
		t.Fatalf("first client: err=%v", err)
	}
	second, err := ResolveClientAddress(ingressRequest("198.51.100.20", "41002"), trusted)
	if err != nil {
		t.Fatalf("second client: err=%v", err)
	}

	if first == second {
		t.Fatalf(
			"two different clients behind the ingress proxy both resolved to %q: "+
				"they share one rate-limit budget, so every signup on the deployment "+
				"competes for the same 5 per 24h",
			first,
		)
	}
	for _, got := range []string{first, second} {
		if got == ingressProxyAddress {
			t.Fatalf(
				"client resolved to the proxy's own address %q; the forwarded client "+
					"identity was discarded",
				got,
			)
		}
	}
	if first != "203.0.113.9:41001" {
		t.Fatalf("first client resolved to %q, want 203.0.113.9:41001", first)
	}
	if second != "198.51.100.20:41002" {
		t.Fatalf("second client resolved to %q, want 198.51.100.20:41002", second)
	}
}

// TestNewClientSessionFromRequestUsesConfiguredTrustedProxies pins the wiring,
// not just the helper. Handlers never call ResolveClientAddress directly; they
// get a session from NewClientSessionFromRequest (router/handler_utils.go:196).
// If that constructor stops consulting the forwarding headers, the helper above
// keeps passing while every real request collapses onto the proxy.
func TestNewClientSessionFromRequestUsesConfiguredTrustedProxies(t *testing.T) {
	original := trustedProxyPrefixes
	defer func() { trustedProxyPrefixes = original }()
	trustedProxyPrefixes = trustedIngress

	firstSession, err := NewClientSessionFromRequest(ingressRequest("203.0.113.9", "41001"))
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	defer firstSession.Cancel()
	secondSession, err := NewClientSessionFromRequest(ingressRequest("198.51.100.20", "41002"))
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	defer secondSession.Cancel()

	if firstSession.ClientAddress == secondSession.ClientAddress {
		t.Fatalf(
			"NewClientSessionFromRequest gave both clients ClientAddress=%q; "+
				"every rate limit keyed on the session address is now fleet-wide",
			firstSession.ClientAddress,
		)
	}
	if firstSession.ClientAddress != "203.0.113.9:41001" {
		t.Fatalf("first session ClientAddress=%q, want 203.0.113.9:41001", firstSession.ClientAddress)
	}
}

// TestUnenumeratedIngressProxyCollapsesEveryClientOntoOneAddress pins the
// shipped default so it cannot drift silently.
//
// BRINGYOUR_TRUSTED_PROXY_CIDRS defaults to loopback only, and it is set
// nowhere in this repository other than its own const declaration. A deployment
// whose ingress proxy is NOT on loopback -- any container-bridge or
// separate-host proxy -- therefore hits the branch asserted below, and every
// client on the deployment shares one address and one budget. That is a
// deployment configuration gap, not a defect in this function: trusting an
// unenumerated private range by default would be strictly worse.
//
// READ THIS BEFORE TREATING A GREEN RUN AS COVERAGE. This test is BLIND to the
// actual remedy. The fix is to set BRINGYOUR_TRUSTED_PROXY_CIDRS on the api
// service to the ingress proxy's subnet, and a Go test process reads an unset
// env var no matter what the deployment does -- so this test keeps passing,
// unchanged, both before and after the fleet-wide 429 is fixed. It detects
// exactly one thing: a change to the compiled-in default. Whether the
// deployment enumerates its proxy cannot be observed from any test in this
// repository.
//
// It cannot be observed from a test, but it IS now observable from the running
// service: reaching the branch below emits an operator-facing error naming the
// peer, which is what TestUnenumeratedIngressProxyIsReportedLoudly pins. That
// report is the reason a deployment can no longer be wedged silently; this test
// still deliberately does not assert the deployment is configured, because it
// cannot.
func TestUnenumeratedIngressProxyCollapsesEveryClientOntoOneAddress(t *testing.T) {
	// capture rather than merely reset, so this test does not print a real
	// operator alert into the go test output
	captureProxyReports(t)
	defaults := trustedProxyPrefixes()

	for _, prefix := range defaults {
		if prefix.Addr().IsLoopback() {
			continue
		}
		t.Fatalf(
			"default trusted proxy set contains the non-loopback prefix %s; "+
				"if that is intentional, update the deployment note on this test",
			prefix,
		)
	}

	first, err := ResolveClientAddress(ingressRequest("203.0.113.9", "41001"), defaults)
	if err != nil {
		t.Fatalf("first client: %v", err)
	}
	second, err := ResolveClientAddress(ingressRequest("198.51.100.20", "41002"), defaults)
	if err != nil {
		t.Fatalf("second client: %v", err)
	}
	if first != ingressProxyAddress || second != ingressProxyAddress {
		t.Fatalf(
			"untrusted ingress proxy no longer collapses clients (%q, %q); the "+
				"default trusted set changed -- revisit the deployment note",
			first, second,
		)
	}
}

// captureProxyReports collects the operator reports a test provokes.
//
// It swaps glogErrorf -- the LAST seam before the log -- and nothing else. An
// earlier version of this helper replaced reportUnenumeratedProxy with a stub
// that re-implemented the rate-limit guard, which meant every test that counted
// reports was counting the stub: the production guard could be deleted outright
// and the whole suite stayed green. Stubbing here puts ResolveClientAddress ->
// the real report function -> the real shouldReportProxy -> the log under one
// seam, so the counts below are counts of what the service would actually
// print.
func captureProxyReports(t *testing.T) *[]string {
	t.Helper()
	reports := []string{}
	original := glogErrorf
	resetProxyReports()
	glogErrorf = func(format string, args ...any) {
		reports = append(reports, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() {
		glogErrorf = original
		resetProxyReports()
	})
	return &reports
}

func reportsMentioning(reports []string, substring string) int {
	count := 0
	for _, report := range reports {
		if strings.Contains(report, substring) {
			count += 1
		}
	}
	return count
}

// TestUnenumeratedIngressProxyIsReportedLoudly is the test for the highest-value
// change in this fix.
//
// The wedged deployment that motivated this suite ran for an unknown length of
// time with BRINGYOUR_TRUSTED_PROXY_CIDRS unset, every client collapsed onto the
// ingress proxy's address, and every signup competing for one 5-per-24h budget.
// Nothing anywhere said so. The only symptom was users being refused, which is
// indistinguishable from the abuse the limiter exists to stop.
//
// A request whose peer is not trusted but which carries a forwarding header is
// exactly that condition: something upstream believes it is a proxy this service
// trusts, and this service disagrees. That must be reported. It must NOT be
// auto-trusted -- see TestUnenumeratedProxyReportDoesNotTrustThePeer.
func TestUnenumeratedIngressProxyIsReportedLoudly(t *testing.T) {
	reports := captureProxyReports(t)
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}

	if _, err := ResolveClientAddress(ingressRequest("203.0.113.9", "41001"), trusted); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(*reports) != 1 {
		t.Fatalf(
			"an untrusted peer sent X-Forwarded-For and %d reports were emitted, want 1: "+
				"the deployment can be wedged onto a single client address with nothing "+
				"in the log saying so",
			len(*reports),
		)
	}
	if !strings.Contains((*reports)[0], "172.18.0.7") {
		t.Fatalf("report does not name the actual TCP peer 172.18.0.7\nreport: %s", (*reports)[0])
	}
	if !strings.Contains((*reports)[0], "X-Forwarded-For") {
		t.Fatalf("report does not name the header that triggered it\nreport: %s", (*reports)[0])
	}

	// X-UR-Forwarded-For is the other header a trusted peer is allowed to set,
	// so it has to be detected too
	urRequest := httptest.NewRequest("POST", "/auth/network-create", nil)
	urRequest.RemoteAddr = "172.18.0.9:53001"
	urRequest.Header.Set("X-UR-Forwarded-For", "203.0.113.9:41001")
	if _, err := ResolveClientAddress(urRequest, trusted); err != nil {
		t.Fatalf("resolve ur-forwarded: %v", err)
	}
	if len(*reports) != 2 {
		t.Fatalf("X-UR-Forwarded-For from an untrusted peer produced %d reports total, want 2", len(*reports))
	}
	if !strings.Contains((*reports)[1], "X-UR-Forwarded-For") {
		t.Fatalf("second report does not name X-UR-Forwarded-For\nreport: %s", (*reports)[1])
	}
}

// TestUnenumeratedProxyReportDoesNotTrustThePeer is the safety half of the
// change above. Reporting the misconfiguration must not repair it: honoring a
// forwarding header from a peer the deployment never enumerated would let any
// client claim any source address and walk out of every per-address limit in
// the service. The report is loud; the resolution is unchanged.
func TestUnenumeratedProxyReportDoesNotTrustThePeer(t *testing.T) {
	captureProxyReports(t)
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}

	got, err := ResolveClientAddress(ingressRequest("203.0.113.9", "41001"), trusted)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != ingressProxyAddress {
		t.Fatalf(
			"an untrusted peer's forwarding header was honored: resolved to %q. Any "+
				"client could now claim any source address and step outside every "+
				"per-address rate limit",
			got,
		)
	}
}

// TestUnenumeratedProxyReportIsRateLimited: the report is emitted on a request
// path, so an unbounded one is a log-flooding hole -- any client can set
// X-Forwarded-For on a directly reachable api. Once per distinct peer per
// interval.
//
// This drives the production guard, not a copy of it: captureProxyReports swaps
// only glogErrorf, so deleting the shouldReportProxy call from
// reportUnenumeratedProxy fails here.
func TestUnenumeratedProxyReportIsRateLimited(t *testing.T) {
	reports := captureProxyReports(t)
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}

	for i := 0; i < 50; i += 1 {
		if _, err := ResolveClientAddress(ingressRequest("203.0.113.9", "41001"), trusted); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
	if len(*reports) != 1 {
		t.Fatalf(
			"50 requests from one untrusted peer produced %d reports, want 1: a client "+
				"can flood the log by repeating a request with a forwarding header",
			len(*reports),
		)
	}

	// a genuinely different peer is worth naming once
	other := httptest.NewRequest("POST", "/auth/network-create", nil)
	other.RemoteAddr = "172.18.0.8:52345"
	other.Header.Set("X-Forwarded-For", "203.0.113.9")
	other.Header.Set("X-Forwarded-Source-Port", "41001")
	if _, err := ResolveClientAddress(other, trusted); err != nil {
		t.Fatalf("resolve other peer: %v", err)
	}
	if len(*reports) != 2 {
		t.Fatalf(
			"a second, distinct untrusted peer produced %d reports total, want 2: an "+
				"operator cannot see which proxies are unenumerated",
			len(*reports),
		)
	}
	if reportsMentioning(*reports, "172.18.0.8") != 1 {
		t.Fatalf("the second peer was not named in any report:\n%s", strings.Join(*reports, "\n"))
	}
}

// TestProxyReportSuppressionIsPerPeerAndPerCondition pins the suppression state
// directly.
//
// It used to be one global "last reported at" instant shared by every peer:
// with the peer set full, whoever called first in each interval took the only
// token, so a caller who could reach the api directly and out-pace the real
// proxy kept the operator's log full of their own address while the genuinely
// wedged proxy was named rarely or never. Per-peer timestamps remove that.
//
// The set is still bounded -- a caller cycling source addresses must not grow
// server memory without limit -- but entries are now pruned once they age out,
// so filling it is no longer permanent for the process lifetime.
func TestProxyReportSuppressionIsPerPeerAndPerCondition(t *testing.T) {
	captureProxyReports(t)

	peer := netip.MustParseAddr("172.18.0.7")
	unenumerated := proxyReportKey{peer: peer, kind: proxyReportUnenumerated}
	unusable := proxyReportKey{peer: peer, kind: proxyReportUnusableHeader}
	now := time.Now()

	if !shouldReportProxy(unenumerated, now) {
		t.Fatal("the first sighting of a peer was suppressed")
	}
	if shouldReportProxy(unenumerated, now.Add(proxyReportInterval-time.Second)) {
		t.Fatal("a repeat inside the interval was reported; the report can be flooded")
	}
	if !shouldReportProxy(unusable, now.Add(time.Second)) {
		t.Fatal(
			"one condition suppressed a different condition for the same peer; the two " +
				"have different remedies and an operator needs to see both",
		)
	}
	if !shouldReportProxy(unenumerated, now.Add(proxyReportInterval)) {
		t.Fatal("no report after the interval elapsed; a wedged deployment goes silent")
	}

	// a busy stranger must not take the wedged proxy's slot
	resetProxyReports()
	stranger := netip.MustParseAddr("198.51.100.1")
	strangerKey := proxyReportKey{peer: stranger, kind: proxyReportUnenumerated}
	for i := 0; i < 100; i += 1 {
		shouldReportProxy(strangerKey, now.Add(time.Duration(i)*time.Millisecond))
	}
	if !shouldReportProxy(unenumerated, now) {
		t.Fatal(
			"a peer that had never been reported was suppressed by another peer's " +
				"traffic: whoever calls first holds the only token and the real wedged " +
				"proxy is never named",
		)
	}

	// a 4-in-6 form of a peer is the same peer: two map slots for one host
	// halves the effective cap, and proxyIsTrusted already unmaps
	resetProxyReports()
	if !shouldReportProxy(proxyReportKey{peer: netip.MustParseAddr("203.0.113.9"), kind: proxyReportUnenumerated}, now) {
		t.Fatal("first sighting suppressed")
	}
	if shouldReportProxy(proxyReportKey{peer: netip.MustParseAddr("::ffff:203.0.113.9"), kind: proxyReportUnenumerated}, now) {
		t.Fatal("the 4-in-6 form of an already-reported peer took a second slot")
	}
}

// TestProxyReportSetIsBoundedAndPrunes: the suppression map is written only
// when a report is actually emitted and pruned when it fills, so a caller
// cycling source addresses can neither grow it without limit nor silence it
// permanently. The second half is the one that matters -- the previous version
// never evicted, so once 1024 peers had been seen the per-peer path was gone
// for the lifetime of the process.
func TestProxyReportSetIsBoundedAndPrunes(t *testing.T) {
	captureProxyReports(t)
	now := time.Now()

	for i := 0; i < proxyReportedMax+512; i += 1 {
		key := proxyReportKey{
			peer: netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1}),
			kind: proxyReportUnenumerated,
		}
		shouldReportProxy(key, now)
	}
	proxyReportMutex.Lock()
	size := len(proxyReportedAt)
	proxyReportMutex.Unlock()
	if proxyReportedMax < size {
		t.Fatalf(
			"the per-peer suppression set grew to %d entries, cap is %d: a caller "+
				"cycling source addresses can grow server memory without limit",
			size, proxyReportedMax,
		)
	}

	// with the set full of entries still inside the interval, the report must
	// not stop entirely
	fresh := proxyReportKey{peer: netip.MustParseAddr("198.51.100.1"), kind: proxyReportUnenumerated}
	if !shouldReportProxy(fresh, now.Add(proxyReportInterval)) {
		t.Fatal("with the peer set full, the report stopped entirely")
	}

	// once the old entries age out they are pruned, so the set does not stay
	// full forever and per-peer reporting comes back
	later := now.Add(2 * proxyReportInterval)
	if !shouldReportProxy(proxyReportKey{peer: netip.MustParseAddr("198.51.100.2"), kind: proxyReportUnenumerated}, later) {
		t.Fatal("a new peer after the interval was suppressed")
	}
	proxyReportMutex.Lock()
	prunedSize := len(proxyReportedAt)
	proxyReportMutex.Unlock()
	if proxyReportedMax <= prunedSize {
		t.Fatalf(
			"the suppression set was still full (%d entries) an interval after every "+
				"entry aged out: it never evicts, so a burst of distinct peers disables "+
				"per-peer reporting for the lifetime of the process",
			prunedSize,
		)
	}
}

// TestProxyReportPruneScanStaysAmortized.
//
// The prune walks the whole suppression map, under the process-global mutex,
// on a code path any caller who can reach the api directly can trigger. Running
// it on every request while the map is full would hand exactly the caller who
// filled it an O(cap) critical section per request -- a contention amplifier
// built out of the flood guard.
//
// An entry only becomes prunable after a whole interval, so scanning more often
// than once per interval cannot free anything the previous scan did not.
func TestProxyReportPruneScanStaysAmortized(t *testing.T) {
	captureProxyReports(t)
	now := time.Now()

	for i := 0; i < proxyReportedMax; i += 1 {
		shouldReportProxy(proxyReportKey{
			peer: netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1}),
			kind: proxyReportUnenumerated,
		}, now)
	}
	proxyReportMutex.Lock()
	before := proxyReportPruneScans
	proxyReportMutex.Unlock()

	// every one of these finds the map full and is refused; none of them can
	// free a slot, because nothing in it has aged out yet
	for i := 0; i < 200; i += 1 {
		shouldReportProxy(proxyReportKey{
			peer: netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}),
			kind: proxyReportUnenumerated,
		}, now)
	}
	proxyReportMutex.Lock()
	after := proxyReportPruneScans
	proxyReportMutex.Unlock()

	if 1 < after-before {
		t.Fatalf(
			"200 requests against a full suppression map ran %d prune scans, want at "+
				"most 1. Each scan walks %d entries holding the process-global report "+
				"mutex, on a path any caller can trigger",
			after-before, proxyReportedMax,
		)
	}

	// and once the entries have aged out, a scan does run again and does free
	// the slots -- an amortized guard that never re-arms is just a leak
	if !shouldReportProxy(proxyReportKey{
		peer: netip.MustParseAddr("203.0.113.9"),
		kind: proxyReportUnenumerated,
	}, now.Add(2*proxyReportInterval)) {
		t.Fatal("after every entry aged out, a new peer was still refused a report")
	}
}

// TestNoProxyReportWithoutForwardingHeaders: a direct client that sends no
// forwarding header is the ordinary case for a service reachable without a
// proxy. Reporting it would bury the real signal.
func TestNoProxyReportWithoutForwardingHeaders(t *testing.T) {
	reports := captureProxyReports(t)
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}

	direct := httptest.NewRequest("POST", "/auth/network-create", nil)
	direct.RemoteAddr = "203.0.113.9:41001"
	got, err := ResolveClientAddress(direct, trusted)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "203.0.113.9:41001" {
		t.Fatalf("direct client resolved to %q", got)
	}
	if len(*reports) != 0 {
		t.Fatalf("an ordinary direct request emitted %d misconfiguration reports", len(*reports))
	}
}

// TestUnenumeratedProxyReportNamesTheRemedyAndItsPrecondition pins the
// operator's only actionable detail, and the correction that motivated this
// round of review.
//
// The report tells an operator to add the peer's subnet to
// BRINGYOUR_TRUSTED_PROXY_CIDRS. The trusted branch used to demand
// X-Forwarded-For paired with X-Forwarded-Source-Port and single-hop, and to
// return an error -- HTTP 500 on every endpoint -- for anything else. Stock
// nginx sends neither the port header nor a single-hop chain. Following this
// message was therefore a way to take the api down. The header contract is now
// permissive enough for a real proxy, and the message states it, including the
// one thing an operator can still get wrong: a proxy that passes a
// client-supplied forwarding header through unchanged lets the client choose
// its own rate-limit bucket.
func TestUnenumeratedProxyReportNamesTheRemedyAndItsPrecondition(t *testing.T) {
	reports := captureProxyReports(t)

	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	if _, err := ResolveClientAddress(ingressRequest("203.0.113.9", "41001"), trusted); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(*reports) != 1 {
		t.Fatalf("want exactly one report, got %d", len(*reports))
	}
	captured := (*reports)[0]

	for _, want := range []string{
		// which peer, which knob, what is trusted today
		"172.18.0.7", trustedProxyCidrsEnvironment, "X-Forwarded-For", "127.0.0.0/8",
		// and what the proxy must actually do once it is trusted, so the
		// operator cannot follow this into a different outage or into a
		// spoofable configuration
		"$proxy_add_x_forwarded_for", "last entry", "unchanged",
	} {
		if !strings.Contains(captured, want) {
			t.Fatalf(
				"the misconfiguration report does not mention %q; an operator reading it "+
					"cannot tell which peer to enumerate, where, or what that proxy has to "+
					"send once it is trusted.\nreport: %s",
				want, captured,
			)
		}
	}
}

// -- the trusted branch: what real proxies actually send -----------------------

// TestStockNginxBareXForwardedForResolvesTheClient is the direct regression for
// the defect this round exists to fix.
//
// nginx's documented recipe is
//
//	proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
//
// and it sends no X-Forwarded-Source-Port at all; ALB, CloudFront and
// Cloudflare are the same. The trusted branch used to answer that with
// `forwarded source requires one IP and one port`, which router.wrap and
// router.wrapWithInput turn into HTTP 500 for EVERY endpoint. An operator who
// followed the report above and enumerated their nginx took the whole api down.
//
// Two assertions, because either one alone is satisfiable by a wrong fix:
// resolving without error (not a 500) AND resolving to the client rather than
// the proxy (not a fleet-wide budget).
func TestStockNginxBareXForwardedForResolvesTheClient(t *testing.T) {
	captureProxyReports(t)
	trusted := trustedIngress()

	got, err := ResolveClientAddress(proxiedRequest("X-Forwarded-For", "203.0.113.9"), trusted)
	if err != nil {
		t.Fatalf(
			"a trusted proxy sending only a bare X-Forwarded-For -- stock nginx, ALB, "+
				"CloudFront, Cloudflare -- was answered with an error: %v. "+
				"router.wrap and router.wrapWithInput turn that into HTTP 500 on every "+
				"endpoint, so enumerating that proxy takes the api down",
			err,
		)
	}
	ip, port, err := server.SplitClientAddress(got)
	if err != nil {
		t.Fatalf("resolved address %q does not split: %v", got, err)
	}
	if ip != "203.0.113.9" {
		t.Fatalf(
			"a bare X-Forwarded-For from a trusted proxy resolved to ip %q (%q); every "+
				"client behind that proxy shares one rate-limit budget",
			ip, got,
		)
	}
	// the port is genuinely unknown here -- no proxy in this shape sends one --
	// and 0 is how that is spelled. Every limiter that matters buckets on the
	// ip alone (server.ClientIpHash), so this costs nothing.
	if port != 0 {
		t.Fatalf("resolved %q, want port 0 for an address with no forwarded port", got)
	}

	// second hop: $proxy_add_x_forwarded_for APPENDS, so a two-proxy path
	// produces a comma. That was the other 500.
	got, err = ResolveClientAddress(
		proxiedRequest("X-Forwarded-For", "203.0.113.9, 172.18.0.9"),
		trusted,
	)
	if err != nil {
		t.Fatalf("a two-hop X-Forwarded-For chain was answered with an error: %v", err)
	}
	if ip, _, _ := server.SplitClientAddress(got); ip != "203.0.113.9" {
		t.Fatalf(
			"a chain whose later hops are all enumerated proxies resolved to %q, want "+
				"the client 203.0.113.9: the trusted hops must be walked past, not "+
				"treated as the client",
			got,
		)
	}
}

// TestClientCannotForgeItsAddressThroughATrustedProxy is the safety half of the
// change above, and the reason the chain is walked from the RIGHT.
//
// X-Forwarded-For is a client-supplied header. A proxy that uses
// $proxy_add_x_forwarded_for appends what it observed, so a client that sends
// "X-Forwarded-For: 6.6.6.6" causes the api to see "6.6.6.6, <client ip>". Read
// left to right -- "the original client" -- every client behind the proxy picks
// its own rate-limit bucket and walks out of every per-address limit in the
// service, which is strictly worse than the shared-budget bug being fixed.
func TestClientCannotForgeItsAddressThroughATrustedProxy(t *testing.T) {
	captureProxyReports(t)
	trusted := trustedIngress()

	forged := "6.6.6.6"
	real := "203.0.113.9"

	for _, chain := range []string{
		// client prepended one address
		forged + ", " + real,
		// client prepended a whole fake chain
		forged + ", 8.8.8.8, 9.9.9.9, " + real,
		// client prepended something that is not an address at all
		"not-an-address, " + real,
		// client prepended an address inside the trusted range, trying to make
		// the walk skip past its real address
		"6.6.6.6, 172.18.0.9, " + real,
	} {
		got, err := ResolveClientAddress(proxiedRequest("X-Forwarded-For", chain), trusted)
		if err != nil {
			t.Fatalf("chain %q: %v", chain, err)
		}
		ip, _, err := server.SplitClientAddress(got)
		if err != nil {
			t.Fatalf("chain %q resolved to %q which does not split: %v", chain, got, err)
		}
		if ip == forged {
			t.Fatalf(
				"a client behind a trusted proxy set X-Forwarded-For: %q and was "+
					"attributed to %q. It just chose its own rate-limit bucket and stepped "+
					"outside every per-address limit in the service",
				chain, ip,
			)
		}
		if ip != real {
			t.Fatalf("chain %q resolved to %q, want the address the proxy appended, %s", chain, ip, real)
		}
	}

	// repeated field lines are equivalent to one comma-joined line (RFC 9110).
	// A proxy that appends a SECOND X-Forwarded-For line rather than extending
	// the first is legal, and reading only the first line would hand back
	// exactly the value the client sent.
	multiLine := httptest.NewRequest("POST", "/auth/network-create", nil)
	multiLine.RemoteAddr = ingressProxyAddress
	multiLine.Header.Add("X-Forwarded-For", forged)
	multiLine.Header.Add("X-Forwarded-For", real)
	got, err := ResolveClientAddress(multiLine, trusted)
	if err != nil {
		t.Fatalf("repeated X-Forwarded-For lines: %v", err)
	}
	if ip, _, _ := server.SplitClientAddress(got); ip != real {
		t.Fatalf(
			"a client sent its own X-Forwarded-For line and the proxy appended a second "+
				"line; the api resolved to %q, want %s. Only the first field line was "+
				"read, so the client chose its own bucket",
			ip, real,
		)
	}
}

// TestUrForwardedForChainIsNotReadAsASingleHop: the same right-to-left rule
// applies to this service's own header. It used to be rejected outright, which
// is safe but is a 500; taking the FIRST element instead would be the forgery
// above wearing a different header name.
func TestUrForwardedForChainIsNotReadAsASingleHop(t *testing.T) {
	captureProxyReports(t)
	trusted := trustedIngress()

	got, err := ResolveClientAddress(
		proxiedRequest("X-UR-Forwarded-For", "6.6.6.6:1, 203.0.113.9:41001"),
		trusted,
	)
	if err != nil {
		t.Fatalf("X-UR-Forwarded-For chain: %v", err)
	}
	if got != "203.0.113.9:41001" {
		t.Fatalf(
			"an X-UR-Forwarded-For chain resolved to %q, want 203.0.113.9:41001: the "+
				"leftmost element is whatever the client supplied",
			got,
		)
	}

	// X-UR-Forwarded-For still wins over X-Forwarded-For when both are present
	both := httptest.NewRequest("POST", "/auth/network-create", nil)
	both.RemoteAddr = ingressProxyAddress
	both.Header.Set("X-UR-Forwarded-For", "203.0.113.9:41001")
	both.Header.Set("X-Forwarded-For", "198.51.100.20")
	got, err = ResolveClientAddress(both, trusted)
	if err != nil {
		t.Fatalf("both headers: %v", err)
	}
	if got != "203.0.113.9:41001" {
		t.Fatalf("with both headers set the resolver returned %q, want the X-UR-Forwarded-For value", got)
	}
}

// TestUnreadableForwardingHeaderFromATrustedPeerDegradesAndIsReported is what
// keeps the change above honest.
//
// Falling back to the peer address instead of erroring cannot produce a wrong
// attribution -- the peer is where the packets came from, the most restrictive
// answer available -- but it silently reinstates the fleet-wide-budget collapse
// this whole file exists to prevent. So it is not silent: a trusted peer whose
// header cannot be read is named, with its own remedy, distinct from the
// unenumerated-proxy remedy.
func TestUnreadableForwardingHeaderFromATrustedPeerDegradesAndIsReported(t *testing.T) {
	reports := captureProxyReports(t)
	trusted := trustedIngress()

	got, err := ResolveClientAddress(proxiedRequest("X-Forwarded-For", "not-an-address"), trusted)
	if err != nil {
		t.Fatalf(
			"an unreadable forwarding header from a trusted peer returned an error (%v); "+
				"router.wrap turns that into HTTP 500 on every endpoint",
			err,
		)
	}
	if got != ingressProxyAddress {
		t.Fatalf(
			"an unreadable header resolved to %q; the only safe fallback is the peer "+
				"address %s",
			got, ingressProxyAddress,
		)
	}
	if len(*reports) != 1 {
		t.Fatalf(
			"a trusted proxy sent a header this service cannot read and %d reports were "+
				"emitted, want 1: every client behind it now shares one rate-limit budget "+
				"and nothing says so",
			len(*reports),
		)
	}
	if !strings.Contains((*reports)[0], "TRUSTED") || !strings.Contains((*reports)[0], "172.18.0.7") {
		t.Fatalf(
			"the report does not identify the trusted peer whose header is broken\nreport: %s",
			(*reports)[0],
		)
	}
	// the header VALUE is attacker-influenced on any proxy that forwards client
	// headers, so it must never reach the log
	if strings.Contains((*reports)[0], "not-an-address") {
		t.Fatalf(
			"the report echoed the attacker-supplied header value; log injection\nreport: %s",
			(*reports)[0],
		)
	}
}

// TestChainOfOnlyTrustedHopsResolvesToThePeerWithoutAReport covers the branch
// where the walk finds no client at all.
//
// Ordinary internal traffic looks like this: one of the deployment's own
// components calls the api through the ingress proxy, so every entry in the
// chain is an enumerated proxy. There is no client to attribute it to, the peer
// address is the correct answer, and nothing is misconfigured -- so this must
// NOT be reported. It shares a return with the unreadable-header case, and if
// the two are ever conflated the log fills up with alerts about traffic that is
// working exactly as intended, which buries the report that matters.
func TestChainOfOnlyTrustedHopsResolvesToThePeerWithoutAReport(t *testing.T) {
	reports := captureProxyReports(t)
	trusted := trustedIngress()

	got, err := ResolveClientAddress(proxiedRequest("X-Forwarded-For", "172.18.0.9"), trusted)
	if err != nil {
		t.Fatalf("a chain of only trusted hops errored: %v", err)
	}
	if got != ingressProxyAddress {
		t.Fatalf(
			"a chain whose every entry is an enumerated proxy resolved to %q, want the "+
				"peer address %s",
			got, ingressProxyAddress,
		)
	}
	if len(*reports) != 0 {
		t.Fatalf(
			"ordinary internal traffic between two enumerated proxies emitted %d "+
				"misconfiguration reports; nothing is misconfigured, and this buries the "+
				"report that matters:\n%s",
			len(*reports), strings.Join(*reports, "\n"),
		)
	}

	// same for a longer all-trusted chain
	got, err = ResolveClientAddress(proxiedRequest("X-Forwarded-For", "172.18.0.9, 172.18.0.10"), trusted)
	if err != nil {
		t.Fatalf("a longer all-trusted chain errored: %v", err)
	}
	if got != ingressProxyAddress {
		t.Fatalf("a longer all-trusted chain resolved to %q, want %s", got, ingressProxyAddress)
	}
	if len(*reports) != 0 {
		t.Fatalf("a longer all-trusted chain emitted %d reports", len(*reports))
	}
}

// TestForwardedSourcePortStillPairsWithBareXForwardedFor pins the one shape
// that already worked, byte for byte. Loosening the contract must not change
// what the warp sidecar's existing pairing resolves to.
func TestForwardedSourcePortStillPairsWithBareXForwardedFor(t *testing.T) {
	captureProxyReports(t)
	trusted := trustedIngress()

	got, err := ResolveClientAddress(ingressRequest("203.0.113.9", "41001"), trusted)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "203.0.113.9:41001" {
		t.Fatalf("X-Forwarded-For + X-Forwarded-Source-Port resolved to %q, want 203.0.113.9:41001", got)
	}

	// ipv6 through the same pairing
	v6 := httptest.NewRequest("POST", "/auth/network-create", nil)
	v6.RemoteAddr = ingressProxyAddress
	v6.Header.Set("X-Forwarded-For", "2001:db8::7")
	v6.Header.Set("X-Forwarded-Source-Port", "41001")
	got, err = ResolveClientAddress(v6, trusted)
	if err != nil {
		t.Fatalf("ipv6 resolve: %v", err)
	}
	if got != "[2001:db8::7]:41001" {
		t.Fatalf("ipv6 X-Forwarded-For resolved to %q, want [2001:db8::7]:41001", got)
	}

	// on a chain the port belongs to whichever hop wrote it and there is no way
	// to tell which, so it is dropped rather than guessed -- but the ADDRESS is
	// still resolved, because dropping the port must not cost the client its
	// own bucket
	chained := httptest.NewRequest("POST", "/auth/network-create", nil)
	chained.RemoteAddr = ingressProxyAddress
	chained.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	chained.Header.Set("X-Forwarded-Source-Port", "41001")
	got, err = ResolveClientAddress(chained, trusted)
	if err != nil {
		t.Fatalf("chained resolve: %v", err)
	}
	if got != "203.0.113.9:0" {
		t.Fatalf(
			"a multi-hop chain took the source port header anyway and resolved to %q, "+
				"want 203.0.113.9:0",
			got,
		)
	}
}

// TestFourInSixAddressesAreOneAddressEverywhere: a v4-mapped v6 peer
// ([::ffff:203.0.113.9]) and the same host arriving as plain ipv4 are one host,
// and every consumer of the resolved address has to agree about that.
//
// proxyIsTrusted unmaps; server.ClientIpHashForAddr does NOT -- it branches on
// addr.Is4(), which is false for the mapped form, and hashes the /56 v6 network
// instead of the /29 v4 one. So leaving the mapped form in place gives one host
// two per-address budgets and two report slots, which halves the effective cap
// on the report set and doubles every rate limit for anyone who can choose
// which form their peer address takes.
func TestFourInSixAddressesAreOneAddressEverywhere(t *testing.T) {
	captureProxyReports(t)
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}

	mapped := httptest.NewRequest("POST", "/auth/network-create", nil)
	mapped.RemoteAddr = "[::ffff:203.0.113.9]:41001"
	got, err := ResolveClientAddress(mapped, trusted)
	if err != nil {
		t.Fatalf("resolve mapped peer: %v", err)
	}
	if got != "203.0.113.9:41001" {
		t.Fatalf(
			"a v4-mapped peer resolved to %q, want 203.0.113.9:41001: it now buckets "+
				"under the ipv6 /56 rule instead of the ipv4 /29 one and holds a second, "+
				"separate rate-limit budget",
			got,
		)
	}

	// the same inside a forwarding header from a trusted peer
	forwarded := httptest.NewRequest("POST", "/auth/network-create", nil)
	forwarded.RemoteAddr = "127.0.0.1:52344"
	forwarded.Header.Set("X-Forwarded-For", "::ffff:203.0.113.9")
	got, err = ResolveClientAddress(forwarded, trusted)
	if err != nil {
		t.Fatalf("resolve mapped forwarded address: %v", err)
	}
	if got != "203.0.113.9:0" {
		t.Fatalf("a v4-mapped forwarded address resolved to %q, want 203.0.113.9:0", got)
	}
}

// TestEveryLoosenedShapeUsedToBeAnHttp500 is the argument that this change
// cannot regress a working deployment, written as a test.
//
// Each input below is a shape the trusted branch now accepts. On the previous
// implementation every one of them returned an error, and router.wrap /
// router.wrapWithInput turn that into HTTP 500. A deployment that reaches any
// of these paths today is a deployment whose api answers 500 to every request,
// so there is no working behaviour for the looser contract to take away -- only
// an outage for it to end.
func TestEveryLoosenedShapeUsedToBeAnHttp500(t *testing.T) {
	captureProxyReports(t)
	trusted := trustedIngress()

	// the previous implementation, verbatim, so the claim is checked rather
	// than asserted in a comment
	oldResolve := func(req *http.Request) error {
		forwarded := strings.TrimSpace(req.Header.Get("X-UR-Forwarded-For"))
		if forwarded != "" {
			if strings.Contains(forwarded, ",") {
				return fmt.Errorf("invalid X-UR-Forwarded-For chain")
			}
			if _, err := parseRequestAddress(forwarded); err != nil {
				return fmt.Errorf("invalid X-UR-Forwarded-For: %w", err)
			}
			return nil
		}
		forwardedIp := strings.TrimSpace(req.Header.Get("X-Forwarded-For"))
		forwardedPort := strings.TrimSpace(req.Header.Get("X-Forwarded-Source-Port"))
		if forwardedIp == "" && forwardedPort == "" {
			return nil
		}
		if forwardedIp == "" || forwardedPort == "" || strings.Contains(forwardedIp, ",") {
			return fmt.Errorf("forwarded source requires one IP and one port")
		}
		return nil
	}

	for _, shape := range []struct {
		what   string
		header string
		value  string
	}{
		{"stock nginx / ALB / CloudFront / Cloudflare", "X-Forwarded-For", "203.0.113.9"},
		{"two nginx hops ($proxy_add_x_forwarded_for appends)", "X-Forwarded-For", "203.0.113.9, 172.18.0.9"},
		{"a client that prepended its own value", "X-Forwarded-For", "6.6.6.6, 203.0.113.9"},
		{"a proxy that writes ip:port into X-Forwarded-For", "X-Forwarded-For", "203.0.113.9:41001"},
		{"an X-UR-Forwarded-For chain", "X-UR-Forwarded-For", "6.6.6.6:1, 203.0.113.9:41001"},
		{"an unreadable header from a trusted proxy", "X-Forwarded-For", "not-an-address"},
	} {
		req := proxiedRequest(shape.header, shape.value)
		if err := oldResolve(req); err == nil {
			t.Fatalf(
				"%s (%s: %s) did NOT error on the previous implementation, so this test "+
					"is no longer evidence that the looser contract only replaces 500s -- "+
					"re-derive the argument",
				shape.what, shape.header, shape.value,
			)
		}
		if _, err := ResolveClientAddress(req, trusted); err != nil {
			t.Fatalf("%s (%s: %s) still errors: %v", shape.what, shape.header, shape.value, err)
		}
	}
}
