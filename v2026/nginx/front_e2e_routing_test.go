package nginx

// End-to-end routing through the assembled front: TLS picks the certificate
// by SNI, then each request routes by its own authority with nginx's
// server_name semantics (D9, review §5), over h1 and h2.

import (
	"net/http"
	"testing"
)

// Over h1 and TLS, the SNI picks the certificate and the Host picks the
// route. A missing SNI gets the default server's certificate yet routes by
// its known Host; a known SNI with another known Host routes to the Host's
// service; an exact name beats a wildcard, the longest wildcard wins, and a
// wildcard covers several labels, as nginx's server_name does; an unknown
// name gets the default server's certificate and its routes. The block, or
// connect's in-process handler, sees the authority it was routed by as its
// Host, and nothing else sees the request.
func TestFrontE2eRoutesH1ByAuthorityAfterSni(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	cases := []struct {
		sni  string
		host string
		path string
		// the vault directory whose leaf the handshake gets
		certificateDir string
		status         int
		// the service whose block gets the request; empty for none
		service string
		// served by connect's in-process handler
		inProcess bool
		// the response body, when the front itself answers
		body string
	}{
		{sni: "", host: "api.service.example", path: "/missing-sni", certificateDir: "test-lb.service.example", status: http.StatusOK, service: "api"},
		{sni: "api.service.example", host: "web.service.example", path: "/other-authority", certificateDir: "api.service.example", status: http.StatusOK, service: "web"},
		{sni: "web.service.example", host: "app.web.service.example", path: "/exact-over-wildcard", certificateDir: "star.service.example", status: http.StatusOK, service: "app"},
		{sni: "web.service.example", host: "x.web.service.example", path: "/wildcard", certificateDir: "star.service.example", status: http.StatusOK, service: "web"},
		{sni: "web.service.example", host: "a.b.web.service.example", path: "/wildcard-labels", certificateDir: "star.service.example", status: http.StatusOK, service: "web"},
		{sni: "web.service.example", host: "x.deep.web.service.example", path: "/longest-wildcard", certificateDir: "star.service.example", status: http.StatusOK, service: "app"},
		{sni: "app.web.service.example", host: "app.web.service.example", path: "/own-certificate", certificateDir: "app.web.service.example", status: http.StatusOK, service: "app"},
		{sni: "x.web.service.example", host: "x.web.service.example", path: "/wildcard-certificate", certificateDir: "star.web.service.example", status: http.StatusOK, service: "web"},
		{sni: "unknown.example", host: "unknown.example", path: "/hidden-example/status", certificateDir: "test-lb.service.example", status: http.StatusOK, body: e2eLbStatusBody},
		{sni: "unknown.example", host: "unknown.example", path: "/no-route", certificateDir: "test-lb.service.example", status: http.StatusNotFound},
		{sni: "", host: "unknown.example", path: "/no-route-either", certificateDir: "test-lb.service.example", status: http.StatusNotFound},
		{sni: "connect.service.example", host: "connect.service.example", path: "/", certificateDir: "star.service.example", status: http.StatusOK, inProcess: true, body: "connect"},
		{sni: "x.connect.service.example", host: "x.connect.service.example", path: "/", certificateDir: "star.connect.service.example", status: http.StatusOK, inProcess: true, body: "connect"},
	}
	for _, c := range cases {
		what := "sni " + c.sni + ", host " + c.host + c.path
		requestCounts := e2e.requestCounts()
		connectRequestCount := len(e2e.connectHandler.recorded())

		client := e2e.newH1Client(t, eth1.httpsAddr, c.sni)
		response, body := e2eDo(t, client, newE2eRequest(t, "GET", "https://"+c.host+c.path, nil))
		if response.ProtoMajor != 1 {
			t.Errorf("%s: HTTP/%d, want h1", what, response.ProtoMajor)
		}
		e2e.checkCertificate(t, what, response.TLS, c.certificateDir)
		if response.StatusCode != c.status {
			t.Errorf("%s: status %d, want %d", what, response.StatusCode, c.status)
		}
		if c.body != "" && string(body) != c.body {
			t.Errorf("%s: body %q, want %q", what, body, c.body)
		}

		requests := e2e.requestsSince(requestCounts)
		connectRequests := e2e.connectHandler.recorded()[connectRequestCount:]
		switch {
		case c.service != "":
			if len(requests) != 1 || requests[0].service != c.service {
				t.Errorf("%s: blocks got %d requests (%v), want one for %s", what, len(requests), e2eRequestServices(requests), c.service)
				continue
			}
			if requests[0].host != c.host || requests[0].requestUri != c.path {
				t.Errorf("%s: the block got host %q and %q", what, requests[0].host, requests[0].requestUri)
			}
			if string(body) != requests[0].service+"/"+requests[0].block {
				t.Errorf("%s: body %q is not the block's answer", what, body)
			}
		case c.inProcess:
			if len(requests) != 0 || len(connectRequests) != 1 {
				t.Errorf("%s: blocks got %d requests and connect %d, want connect one", what, len(requests), len(connectRequests))
				continue
			}
			if connectRequests[0].host != c.host || connectRequests[0].requestUri != c.path || !connectRequests[0].overTls {
				t.Errorf("%s: connect got host %q, %q, over TLS %t", what, connectRequests[0].host, connectRequests[0].requestUri, connectRequests[0].overTls)
			}
		default:
			if len(requests) != 0 || len(connectRequests) != 0 {
				t.Errorf("%s: blocks got %d requests and connect %d, want none", what, len(requests), len(connectRequests))
			}
		}
	}
}

// Over h2, each stream routes by its own :authority, so one connection,
// whose SNI chose the api certificate, carries requests for several
// services, the lb's routes and the default server (D9, review §5). Every
// forwarded stream names the one peer of the connection.
func TestFrontE2eRoutesH2StreamsByTheirOwnAuthority(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	clientConn, localAddr := e2e.newH2ClientConn(t, eth1.httpsAddr, "api.service.example")
	streams := []struct {
		authority string
		path      string
		status    int
		// the service whose block gets the stream; empty for none
		service string
		// the response body, when the front itself answers
		body string
	}{
		{authority: "api.service.example", path: "/h2-api", status: http.StatusOK, service: "api"},
		{authority: "web.service.example", path: "/h2-web", status: http.StatusOK, service: "web"},
		{authority: "x.deep.web.service.example", path: "/h2-longest-wildcard", status: http.StatusOK, service: "app"},
		{authority: "a.b.web.service.example", path: "/h2-wildcard-labels", status: http.StatusOK, service: "web"},
		{authority: "test-lb.other.example", path: "/hidden-example/status", status: http.StatusOK, body: e2eLbStatusBody},
		{authority: "unknown.example", path: "/h2-unknown", status: http.StatusNotFound},
		{authority: "api.service.example", path: "/h2-api-again", status: http.StatusOK, service: "api"},
	}
	forwardedFors := map[string]bool{}
	for _, stream := range streams {
		what := "authority " + stream.authority + stream.path
		requestCounts := e2e.requestCounts()
		request := newE2eRequest(t, "GET", "https://api.service.example"+stream.path, nil)
		request.Host = stream.authority
		response, body := e2eRoundTripH2(t, clientConn, request)
		if response.ProtoMajor != 2 {
			t.Errorf("%s: HTTP/%d, want h2", what, response.ProtoMajor)
		}
		e2e.checkCertificate(t, what, response.TLS, "api.service.example")
		if response.StatusCode != stream.status {
			t.Errorf("%s: status %d, want %d", what, response.StatusCode, stream.status)
		}
		if stream.body != "" && string(body) != stream.body {
			t.Errorf("%s: body %q, want %q", what, body, stream.body)
		}
		requests := e2e.requestsSince(requestCounts)
		if stream.service == "" {
			if len(requests) != 0 {
				t.Errorf("%s: blocks got %d requests, want none", what, len(requests))
			}
			continue
		}
		if len(requests) != 1 || requests[0].service != stream.service {
			t.Errorf("%s: blocks got %d requests (%v), want one for %s", what, len(requests), e2eRequestServices(requests), stream.service)
			continue
		}
		if requests[0].host != stream.authority || requests[0].requestUri != stream.path || requests[0].proto != "HTTP/1.1" {
			t.Errorf("%s: the block got host %q, %q over %s", what, requests[0].host, requests[0].requestUri, requests[0].proto)
		}
		forwardedFors[requests[0].header.Get("X-UR-Forwarded-For")] = true
	}
	if len(forwardedFors) != 1 || !forwardedFors[localAddr] {
		t.Errorf("the streams of one connection were forwarded for %v, want its peer %s", forwardedFors, localAddr)
	}

	// an h2 connection that names no server gets the default server's
	// certificate, and its streams still route by authority
	noSniClientConn, _ := e2e.newH2ClientConn(t, eth1.httpsAddr, "")
	requestCounts := e2e.requestCounts()
	request := newE2eRequest(t, "GET", "https://web.service.example/h2-missing-sni", nil)
	response, body := e2eRoundTripH2(t, noSniClientConn, request)
	e2e.checkCertificate(t, "h2 with no SNI", response.TLS, "test-lb.service.example")
	requests := e2e.requestsSince(requestCounts)
	if response.StatusCode != http.StatusOK || string(body) != "web/g1" || len(requests) != 1 || requests[0].host != "web.service.example" {
		t.Errorf("h2 with no SNI: %d %q, the blocks got %v", response.StatusCode, body, e2eRequestServices(requests))
	}
}

// The services of some requests, for a failure message.
func e2eRequestServices(requests []*e2eUpstreamRequest) []string {
	services := []string{}
	for _, request := range requests {
		services = append(services, request.service+"/"+request.block)
	}
	return services
}
