package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/session"
)

// Regression suite for the HTTP 500 an operator could be talked into.
//
// session.ResolveClientAddress used to require, from a trusted peer, either
// X-UR-Forwarded-For as ip:port or X-Forwarded-For paired with
// X-Forwarded-Source-Port and single-hop. Anything else was an ERROR, and both
// wrap and wrapWithInput answer a session construction failure with
//
//	http.Error(w, err.Error(), http.StatusInternalServerError)
//
// -- for every endpoint on the api, not just signup.
//
// Meanwhile session's misconfiguration report tells an operator whose ingress
// proxy is not enumerated to add its subnet to BRINGYOUR_TRUSTED_PROXY_CIDRS.
// Stock nginx (proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for),
// ALB, CloudFront and Cloudflare all send a bare X-Forwarded-For and no source
// port, and $proxy_add_x_forwarded_for APPENDS, so a second hop adds the comma
// that was also rejected. Following the report therefore turned a degraded
// deployment (one shared rate-limit budget) into a dead one (500 on every
// request).
//
// These tests assert at the HTTP boundary, because that is where the defect
// was: a unit test on ResolveClientAddress checks the input to the thing that
// produced the 500, not the 500.
//
// They rely on the shipped default trusted set, 127.0.0.0/8 + ::1/128, so a
// loopback RemoteAddr is a TRUSTED peer here with no env manipulation.

// forwardedAddressRequest is a request as a trusted ingress proxy on loopback
// would deliver it.
func forwardedAddressRequest(t *testing.T, headers map[string]string) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", "/auth/network-create", strings.NewReader("{}"))
	req.RemoteAddr = "127.0.0.1:52344"
	for header, value := range headers {
		req.Header.Set(header, value)
	}
	return req
}

// runWrapped drives the real handler wrappers and reports what the client would
// have received, plus the client address the impl was handed.
func runWrapped(req *http.Request) (statusCode int, body string, clientAddress string) {
	recorder := httptest.NewRecorder()
	wrap(
		func(clientSession *session.ClientSession) (string, error) {
			clientAddress = clientSession.ClientAddress
			return "ok", nil
		},
		recorder,
		req,
	)
	return recorder.Code, strings.TrimRight(recorder.Body.String(), "\n"), clientAddress
}

func runWrappedWithInput(req *http.Request) (statusCode int, body string, clientAddress string) {
	recorder := httptest.NewRecorder()
	wrapWithInput(
		RequestBodyFormatter,
		func(input map[string]any, clientSession *session.ClientSession) (string, error) {
			clientAddress = clientSession.ClientAddress
			return "ok", nil
		},
		recorder,
		req,
	)
	return recorder.Code, strings.TrimRight(recorder.Body.String(), "\n"), clientAddress
}

// TestEnumeratedProxySendingOnlyBareXForwardedForIsNotA500 is the proof
// obligation for this fix, stated the way the defect was.
func TestEnumeratedProxySendingOnlyBareXForwardedForIsNotA500(t *testing.T) {
	for _, shape := range []struct {
		what    string
		headers map[string]string
	}{
		{
			"stock nginx / ALB / CloudFront / Cloudflare: bare X-Forwarded-For, no source port",
			map[string]string{"X-Forwarded-For": "203.0.113.9"},
		},
		{
			"two hops: $proxy_add_x_forwarded_for appends, producing a comma",
			map[string]string{"X-Forwarded-For": "203.0.113.9, 127.0.0.9"},
		},
		{
			"a client that prepended its own X-Forwarded-For before the proxy appended",
			map[string]string{"X-Forwarded-For": "6.6.6.6, 203.0.113.9"},
		},
		{
			"a proxy that writes ip:port into X-Forwarded-For",
			map[string]string{"X-Forwarded-For": "203.0.113.9:41001"},
		},
		{
			"a forwarding header this service cannot read at all",
			map[string]string{"X-Forwarded-For": "not-an-address"},
		},
		{
			"an X-UR-Forwarded-For chain",
			map[string]string{"X-UR-Forwarded-For": "6.6.6.6:1, 203.0.113.9:41001"},
		},
	} {
		for _, wrapper := range []struct {
			name string
			run  func(*http.Request) (int, string, string)
		}{
			{"wrap", runWrapped},
			{"wrapWithInput", runWrappedWithInput},
		} {
			statusCode, body, _ := wrapper.run(forwardedAddressRequest(t, shape.headers))
			if statusCode == http.StatusInternalServerError {
				t.Fatalf(
					"%s: %s answered HTTP 500 (%q).\n"+
						"An operator who read the unenumerated-proxy report, added their "+
						"ingress proxy's subnet to BRINGYOUR_TRUSTED_PROXY_CIDRS and changed "+
						"nothing else has just taken the entire api down -- every endpoint, "+
						"not only signup",
					wrapper.name, shape.what, body,
				)
			}
			if statusCode != http.StatusOK {
				t.Fatalf("%s: %s answered HTTP %d (%q), want 200", wrapper.name, shape.what, statusCode, body)
			}
		}
	}
}

// TestEnumeratingAProxyActuallySeparatesItsClients: not answering 500 is only
// half of it. If enumerating the proxy left every client resolving to the
// proxy's own address, the operator would have followed the report, kept the
// api up, and still had one 5-per-24h signup budget for the whole fleet -- the
// exact bug the report exists to name.
func TestEnumeratingAProxyActuallySeparatesItsClients(t *testing.T) {
	_, _, first := runWrapped(forwardedAddressRequest(t, map[string]string{
		"X-Forwarded-For": "203.0.113.9",
	}))
	_, _, second := runWrapped(forwardedAddressRequest(t, map[string]string{
		"X-Forwarded-For": "198.51.100.20",
	}))

	if first == second {
		t.Fatalf(
			"two clients behind the enumerated proxy both reached the handler with "+
				"ClientAddress=%q; every per-address rate limit in the service is now "+
				"one fleet-wide limit",
			first,
		)
	}
	for _, got := range []string{first, second} {
		if strings.HasPrefix(got, "127.0.0.1:") {
			t.Fatalf(
				"a client behind the enumerated proxy reached the handler with the "+
					"proxy's own address %q; the forwarded identity was discarded",
				got,
			)
		}
	}
	if first != "203.0.113.9:0" {
		t.Fatalf("handler saw ClientAddress=%q, want 203.0.113.9:0", first)
	}
}

// TestForgedForwardedAddressDoesNotReachTheHandler is the safety half at the
// same boundary. X-Forwarded-For is client-supplied; a proxy that appends means
// everything left of the appended entry is the client's own text. If that
// reached the handler, every client behind the proxy would pick its own
// rate-limit bucket -- strictly worse than the shared-budget bug.
func TestForgedForwardedAddressDoesNotReachTheHandler(t *testing.T) {
	_, _, clientAddress := runWrapped(forwardedAddressRequest(t, map[string]string{
		"X-Forwarded-For": "6.6.6.6, 203.0.113.9",
	}))
	if strings.HasPrefix(clientAddress, "6.6.6.6") {
		t.Fatalf(
			"a client behind the trusted proxy prepended X-Forwarded-For: 6.6.6.6 and "+
				"the handler was given ClientAddress=%q. It just chose its own rate-limit "+
				"bucket",
			clientAddress,
		)
	}
	if clientAddress != "203.0.113.9:0" {
		t.Fatalf("handler saw ClientAddress=%q, want the address the proxy appended, 203.0.113.9:0", clientAddress)
	}

	// an unreadable header must fall back to the peer, never to whatever the
	// client wrote
	_, _, clientAddress = runWrapped(forwardedAddressRequest(t, map[string]string{
		"X-Forwarded-For": "not-an-address",
	}))
	if clientAddress != "127.0.0.1:52344" {
		t.Fatalf(
			"an unreadable forwarding header gave the handler ClientAddress=%q, want the "+
				"peer address 127.0.0.1:52344",
			clientAddress,
		)
	}
}

// TestSecondForwardingHeaderDoesNotReachTheHandler is the HTTP-boundary form of
// session.TestASecondForwardingHeaderCannotChooseTheBucket.
//
// A proxy overwrites the header it sets and passes the client's other headers
// through by default, so "the api reads two forwarding headers and honors the
// first one it finds" was a bucket the client could pick, on whichever
// deployment shape did not own that header. Asserted here because the value
// that matters is the ClientAddress the impl is HANDED -- that is what every
// per-address limit keys on -- and because the refusal must not have turned
// into the 500 this suite exists to prevent.
func TestSecondForwardingHeaderDoesNotReachTheHandler(t *testing.T) {
	for _, shape := range []struct {
		what    string
		headers map[string]string
	}{
		{
			"proxy owns X-Forwarded-For, client adds X-UR-Forwarded-For",
			map[string]string{
				"X-Forwarded-For":    "203.0.113.9",
				"X-UR-Forwarded-For": "6.6.6.6:1234",
			},
		},
		{
			"proxy owns X-UR-Forwarded-For, client adds X-Forwarded-For",
			map[string]string{
				"X-UR-Forwarded-For": "203.0.113.9:41001",
				"X-Forwarded-For":    "6.6.6.6",
			},
		},
		// The two below are the shapes the first pass of the conflict rule
		// let through, at the boundary where it matters. The proxy's header
		// is present and names only an address inside an enumerated CIDR
		// (here 127.0.0.0/8, the shipped default), so it makes no claim about
		// a client -- and a header that makes no claim used to be unable to
		// contradict the client's, which handed the client the address it
		// wrote. This is the shape a client reaches on the beta deploy if
		// X-UR-Forwarded-For is ever passed through.
		{
			"proxy's X-Forwarded-For names only an enumerated hop, client adds X-UR-Forwarded-For",
			map[string]string{
				"X-Forwarded-For":    "127.0.0.9",
				"X-UR-Forwarded-For": "6.6.6.6:1234",
			},
		},
		{
			"proxy's X-UR-Forwarded-For names only an enumerated hop, client adds X-Forwarded-For",
			map[string]string{
				"X-UR-Forwarded-For": "127.0.0.9:5000",
				"X-Forwarded-For":    "6.6.6.6",
			},
		},
	} {
		for _, run := range []struct {
			wrapper string
			call    func(*http.Request) (int, string, string)
		}{
			{"wrap", runWrapped},
			{"wrapWithInput", runWrappedWithInput},
		} {
			statusCode, body, clientAddress := run.call(forwardedAddressRequest(t, shape.headers))
			if statusCode != http.StatusOK {
				t.Fatalf("%s: %s answered HTTP %d (%q), want 200", shape.what, run.wrapper, statusCode, body)
			}
			if strings.HasPrefix(clientAddress, "6.6.6.6") {
				t.Fatalf(
					"%s: %s handed the impl ClientAddress=%q. A client behind the trusted "+
						"proxy just chose its own rate-limit bucket by setting the forwarding "+
						"header the proxy does not overwrite",
					shape.what, run.wrapper, clientAddress,
				)
			}
			if clientAddress != "127.0.0.1:52344" {
				t.Fatalf(
					"%s: %s handed the impl ClientAddress=%q, want the peer address "+
						"127.0.0.1:52344: with two headers naming two clients neither can be "+
						"shown to be the proxy's",
					shape.what, run.wrapper, clientAddress,
				)
			}
		}
	}

	// and the deployment whose proxy sets BOTH headers correctly keeps the
	// address and the port it had before the rule above existed
	statusCode, body, clientAddress := runWrapped(forwardedAddressRequest(t, map[string]string{
		"X-UR-Forwarded-For": "203.0.113.9:41001",
		"X-Forwarded-For":    "6.6.6.6, 203.0.113.9",
	}))
	if statusCode != http.StatusOK {
		t.Fatalf("a proxy setting both headers consistently answered HTTP %d (%q), want 200", statusCode, body)
	}
	if clientAddress != "203.0.113.9:41001" {
		t.Fatalf(
			"a proxy that sets both headers correctly handed the impl ClientAddress=%q, "+
				"want 203.0.113.9:41001. Refusing on any difference between the two headers "+
				"would take the client address away from a working deployment",
			clientAddress,
		)
	}
}

// TestUntrustedPeerForwardingHeaderIsIgnoredAtTheHandler: the whole contract
// only holds because the peer must be enumerated first. A request arriving from
// a non-loopback peer carries no authority at all, whatever it claims.
func TestUntrustedPeerForwardingHeaderIsIgnoredAtTheHandler(t *testing.T) {
	req := forwardedAddressRequest(t, map[string]string{"X-Forwarded-For": "203.0.113.9"})
	req.RemoteAddr = "192.0.2.44:54321"

	statusCode, body, clientAddress := runWrapped(req)
	if statusCode != http.StatusOK {
		t.Fatalf("untrusted peer answered HTTP %d (%q), want 200", statusCode, body)
	}
	if clientAddress != "192.0.2.44:54321" {
		t.Fatalf(
			"a forwarding header from an UNTRUSTED peer was honored: the handler saw "+
				"ClientAddress=%q. Any client could now claim any source address and step "+
				"outside every per-address rate limit in the service",
			clientAddress,
		)
	}
}
