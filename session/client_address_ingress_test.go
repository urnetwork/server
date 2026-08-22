package session

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// Regression suite for wrongful 429/503 on account creation.
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

// TestResolveClientAddressSeparatesClientsBehindTrustedIngressProxy is the
// highest-value assertion in this suite: two unrelated signups arriving through
// the same proxy must not resolve to the same address. A resolver that returns
// the proxy address for both gives the entire fleet a single 5-per-24h signup
// budget.
func TestResolveClientAddressSeparatesClientsBehindTrustedIngressProxy(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix(ingressProxyCidr)}

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
	trustedProxyPrefixes = func() []netip.Prefix {
		return []netip.Prefix{netip.MustParsePrefix(ingressProxyCidr)}
	}

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
	resetUnenumeratedProxyReports()
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

// capturedProxyReport is one operator-facing report of an unenumerated proxy.
type capturedProxyReport struct {
	peer   netip.Addr
	header string
}

// captureProxyReports swaps the reporter for the duration of a test and clears
// the suppression state on both sides, so one test cannot silence the next.
func captureProxyReports(t *testing.T) *[]capturedProxyReport {
	t.Helper()
	reports := []capturedProxyReport{}
	original := reportUnenumeratedProxy
	resetUnenumeratedProxyReports()
	reportUnenumeratedProxy = func(peer netip.Addr, header string, trusted []netip.Prefix) {
		if !shouldReportUnenumeratedProxy(peer, time.Now()) {
			return
		}
		reports = append(reports, capturedProxyReport{peer: peer, header: header})
	}
	t.Cleanup(func() {
		reportUnenumeratedProxy = original
		resetUnenumeratedProxyReports()
	})
	return &reports
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
	if got := (*reports)[0].peer.String(); got != "172.18.0.7" {
		t.Fatalf("report named peer %q, want the actual TCP peer 172.18.0.7", got)
	}
	if (*reports)[0].header != "X-Forwarded-For" {
		t.Fatalf("report named header %q, want X-Forwarded-For", (*reports)[0].header)
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
	if (*reports)[1].header != "X-UR-Forwarded-For" {
		t.Fatalf("second report named header %q, want X-UR-Forwarded-For", (*reports)[1].header)
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
// X-Forwarded-For on a directly reachable api. Once per distinct peer, then no
// more than once per interval.
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
}

// TestUnenumeratedProxyReportSurvivesTheBoundedPeerSet pins the suppression
// state directly. The per-peer set is bounded, so a caller cycling source
// addresses cannot grow it without limit -- but once it is full the report must
// keep firing on the interval, otherwise a deployment that becomes wedged later
// goes quiet again, which is the exact failure this change exists to prevent.
func TestUnenumeratedProxyReportSurvivesTheBoundedPeerSet(t *testing.T) {
	captureProxyReports(t)

	peer := netip.MustParseAddr("172.18.0.7")
	now := time.Now()
	if !shouldReportUnenumeratedProxy(peer, now) {
		t.Fatal("the first sighting of a peer was suppressed")
	}
	if shouldReportUnenumeratedProxy(peer, now.Add(unenumeratedProxyReportInterval-time.Second)) {
		t.Fatal("a repeat inside the interval was reported; the report can be flooded")
	}
	if !shouldReportUnenumeratedProxy(peer, now.Add(unenumeratedProxyReportInterval)) {
		t.Fatal("no report after the interval elapsed; a wedged deployment goes silent")
	}

	// fill the bounded set, then confirm the interval path still fires
	resetUnenumeratedProxyReports()
	for i := 0; i < unenumeratedProxyReportedMax+16; i += 1 {
		shouldReportUnenumeratedProxy(netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1}), now)
	}
	unenumeratedProxyMutex.Lock()
	size := len(unenumeratedProxyReported)
	unenumeratedProxyMutex.Unlock()
	if unenumeratedProxyReportedMax < size {
		t.Fatalf(
			"the per-peer suppression set grew to %d entries, cap is %d: a caller "+
				"cycling source addresses can grow server memory without limit",
			size, unenumeratedProxyReportedMax,
		)
	}
	if !shouldReportUnenumeratedProxy(netip.MustParseAddr("198.51.100.1"), now.Add(unenumeratedProxyReportInterval)) {
		t.Fatal("with the peer set full, the interval path stopped reporting entirely")
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

// TestUnenumeratedProxyReportNamesTheEnvironmentVariable pins the operator's
// only actionable detail. A report that says something is wrong without naming
// the peer and the variable to set is not the fix -- the deployment sat wedged
// because nobody knew which knob to turn.
func TestUnenumeratedProxyReportNamesTheEnvironmentVariable(t *testing.T) {
	resetUnenumeratedProxyReports()
	defer resetUnenumeratedProxyReports()

	captured := ""
	original := glogErrorf
	glogErrorf = func(format string, args ...any) {
		captured = fmt.Sprintf(format, args...)
	}
	defer func() { glogErrorf = original }()

	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	if _, err := ResolveClientAddress(ingressRequest("203.0.113.9", "41001"), trusted); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	for _, want := range []string{"172.18.0.7", trustedProxyCidrsEnvironment, "X-Forwarded-For", "127.0.0.0/8"} {
		if !strings.Contains(captured, want) {
			t.Fatalf(
				"the misconfiguration report does not mention %q; an operator reading it "+
					"cannot tell which peer to enumerate or where.\nreport: %s",
				want, captured,
			)
		}
	}
}
