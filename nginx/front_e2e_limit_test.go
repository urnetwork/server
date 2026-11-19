package nginx

// End-to-end tests of the D8 limits: nginx's per-address zones are per
// interface, so the front keys its state by interface and address, and one
// source on two interfaces never shares a quota (review, "Limit identity").

import (
	"net/http"
	"strconv"
	"testing"
)

// The request zone is shared by an interface's services and counted per
// interface: with two tokens per address (burst 1), a source's third request
// on eth1 is a 429 whichever service it is for, while eth2 still admits its
// own two. A refused request reaches no block, and carries the service
// name's headers.
func TestFrontE2eLimitsRequestsPerInterfaceAndAddress(t *testing.T) {
	e2e := newE2eFront(t, nil, func(routeTable *RouteTable) {
		for _, routeInterface := range routeTable.Interfaces {
			// one token per 100 minutes: none comes back within the test
			routeInterface.RateLimit.RequestsPerMinute = 0.01
			routeInterface.RateLimit.Burst = 1
			routeInterface.RateLimit.NetConnections = 0
		}
	})
	for _, interfaceName := range []string{"eth1", "eth2"} {
		e2eInterface := e2e.iface(t, interfaceName)
		apiClient := e2e.newH1Client(t, e2eInterface.httpsAddr, "api.service.example")
		webClient := e2e.newH1Client(t, e2eInterface.httpsAddr, "web.service.example")
		requests := []struct {
			client *http.Client
			url    string
			status int
		}{
			{client: apiClient, url: "https://api.service.example/first", status: http.StatusOK},
			{client: webClient, url: "https://web.service.example/second", status: http.StatusOK},
			{client: apiClient, url: "https://api.service.example/third", status: http.StatusTooManyRequests},
			{client: webClient, url: "https://web.service.example/fourth", status: http.StatusTooManyRequests},
		}
		for _, request := range requests {
			requestCounts := e2e.requestCounts()
			response, _ := e2eDo(t, request.client, newE2eRequest(t, "GET", request.url, nil))
			if response.StatusCode != request.status {
				t.Errorf("%s %s: %d, want %d", interfaceName, request.url, response.StatusCode, request.status)
			}
			forwarded := len(e2e.requestsSince(requestCounts))
			if request.status == http.StatusOK && forwarded != 1 || request.status != http.StatusOK && forwarded != 0 {
				t.Errorf("%s %s: %d requests reached a block", interfaceName, request.url, forwarded)
			}
			if request.status == http.StatusTooManyRequests && response.Header.Get("Strict-Transport-Security") == "" {
				t.Errorf("%s %s: the 429 lacks the service name's security headers", interfaceName, request.url)
			}
		}
	}
}

// The concurrency cap counts each (interface, address): with
// net_connections 1, a request held at a block fills eth1's slot for its
// address, so the address's next request on eth1 is a 429, while another
// address on eth1, and the same address on eth2, are served. Once the held
// request ends, eth1 serves the address again.
func TestFrontE2eCapsConcurrencyPerInterfaceAndAddress(t *testing.T) {
	e2e := newE2eFront(t, nil, func(routeTable *RouteTable) {
		for _, routeInterface := range routeTable.Interfaces {
			routeInterface.RateLimit.RequestsPerMinute = 0
			routeInterface.RateLimit.NetConnections = 1
		}
	})
	eth1 := e2e.iface(t, "eth1")
	eth2 := e2e.iface(t, "eth2")
	web := e2e.upstream(t, "web", "g1")
	hold := newE2eHold(t)
	web.setHandler(func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
		if request.requestUri == "/held" {
			hold.wait(r)
		}
		w.WriteHeader(http.StatusOK)
	})

	eth1Client := e2e.newH1Client(t, eth1.httpsAddr, "web.service.example")
	heldStatuses := make(chan int, 1)
	go func() {
		response, err := eth1Client.Do(newE2eRequest(t, "GET", "https://web.service.example/held", nil))
		if err != nil {
			heldStatuses <- 0
			return
		}
		response.Body.Close()
		heldStatuses <- response.StatusCode
	}()
	web.awaitRequests(t, 1)

	response, _ := e2eDo(t, e2e.newH1Client(t, eth1.httpsAddr, "web.service.example"), newE2eRequest(t, "GET", "https://web.service.example/same-address", nil))
	if response.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the address's second request on eth1 = %d, want 429", response.StatusCode)
	}
	response, _ = e2eDo(t, e2e.newH1Client(t, eth2.httpsAddr, "web.service.example"), newE2eRequest(t, "GET", "https://web.service.example/other-interface", nil))
	if response.StatusCode != http.StatusOK {
		t.Errorf("the address on eth2 = %d, want 200", response.StatusCode)
	}
	if eth1.httpsIpv6Addr != "" {
		response, _ = e2eDo(t, e2e.newH1Client(t, eth1.httpsIpv6Addr, "web.service.example"), newE2eRequest(t, "GET", "https://web.service.example/other-address", nil))
		if response.StatusCode != http.StatusOK {
			t.Errorf("another address on eth1 = %d, want 200", response.StatusCode)
		}
	} else {
		t.Log("no loopback IPv6: a second address is not checked")
	}

	hold.release()
	if status := receiveE2e(t, heldStatuses, "held response"); status != http.StatusOK {
		t.Errorf("the held request = %d, want 200", status)
	}
	eth1.httpsServed.await(t, "/held")
	response, _ = e2eDo(t, eth1Client, newE2eRequest(t, "GET", "https://web.service.example/after", nil))
	if response.StatusCode != http.StatusOK {
		t.Errorf("the address on eth1 after the held request ended = %d, want 200", response.StatusCode)
	}
	for _, request := range web.recorded() {
		if request.requestUri == "/same-address" {
			t.Errorf("a refused request reached the block")
		}
	}
}

// A head of nginx's max_headers, 1000 lines, reaches the block over h1 and
// h2, and one more line is the front's own 400, as nginx answers it: the
// public servers parse twice that many lines (Front.ConfigureServer), where
// Go's default 500 would answer 431 first.
func TestFrontE2eServesNginxMaxHeadersLines(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	maxRequestHeaderLines := e2e.settings.Http.MaxRequestHeaderLines

	// h1: the request line, Host, then the rest of the lines
	for _, c := range []struct {
		lineCount int
		status    int
	}{
		{lineCount: maxRequestHeaderLines, status: http.StatusOK},
		{lineCount: maxRequestHeaderLines + 1, status: http.StatusBadRequest},
	} {
		headLines := []string{"GET /lines-" + strconv.Itoa(c.lineCount) + " HTTP/1.1", "Host: api.service.example"}
		for i := range c.lineCount - 1 {
			headLines = append(headLines, "X-Line-"+strconv.Itoa(i)+": v")
		}
		requestCounts := e2e.requestCounts()
		response, _ := e2e.dialRaw(t, eth1.httpsAddr, "api.service.example").exchange(t, headLines...)
		if response.StatusCode != c.status {
			t.Errorf("h1 with %d header lines = %d, want %d", c.lineCount, response.StatusCode, c.status)
		}
		if reached := len(e2e.requestsSince(requestCounts)) != 0; reached != (c.status == http.StatusOK) {
			t.Errorf("h1 with %d header lines reached a block %t", c.lineCount, reached)
		}
	}

	// h2: the pseudo-headers, then the regular fields
	clientConn, _ := e2e.newH2ClientConn(t, eth1.httpsAddr, "api.service.example")
	for _, c := range []struct {
		lineCount int
		status    int
	}{
		{lineCount: maxRequestHeaderLines, status: http.StatusOK},
		{lineCount: maxRequestHeaderLines + 1, status: http.StatusBadRequest},
	} {
		request := newE2eRequest(t, "GET", "https://api.service.example/h2-lines-"+strconv.Itoa(c.lineCount), nil)
		// the client adds no field of its own to a request with a
		// User-Agent and an Accept-Encoding
		request.Header.Set("User-Agent", "e2e")
		request.Header.Set("Accept-Encoding", "identity")
		for i := range c.lineCount - len(request.Header) {
			request.Header.Set("X-Line-"+strconv.Itoa(i), "v")
		}
		requestCounts := e2e.requestCounts()
		response, _ := e2eRoundTripH2(t, clientConn, request)
		if response.StatusCode != c.status {
			t.Errorf("h2 with %d header fields = %d, want %d", c.lineCount, response.StatusCode, c.status)
		}
		if reached := len(e2e.requestsSince(requestCounts)) != 0; reached != (c.status == http.StatusOK) {
			t.Errorf("h2 with %d header fields reached a block %t", c.lineCount, reached)
		}
	}
}
