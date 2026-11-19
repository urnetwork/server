package nginx

// End-to-end tests of what the assembled front records (D17).

import (
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// D17: the front counts each request by the service it is for, its
// protocol, its status class and the block that answered. A balanced
// forwarded request names the block the balancer chose, a per-block lb
// route its block, and connect's in-process handler "local"; no other label
// of the service moves.
func TestFrontE2eCountsRequestsByTheBlockThatAnswered(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	h2ClientConn, _ := e2e.newH2ClientConn(t, eth1.httpsAddr, "api.service.example")
	cases := []struct {
		protocol string
		host     string
		path     string
		service  string
		// the block label the count names; empty for the block that got
		// the request
		block string
	}{
		{protocol: "h1", host: "web.service.example", path: "/counted-web", service: "web"},
		{protocol: "h1", host: "api.service.example", path: "/counted-api", service: "api"},
		{protocol: "h2", host: "api.service.example", path: "/counted-api-h2", service: "api"},
		{protocol: "h1", host: "test-lb.service.example", path: "/hidden-example/by/b/api/g2/counted-block", service: "api", block: "g2"},
		{protocol: "h1", host: "connect.service.example", path: "/?counted-connect", service: "connect", block: routeHttpInProcessBlock},
	}
	for _, c := range cases {
		what := c.protocol + " " + c.host + c.path
		routeService, ok := e2e.routeTable.Service(c.service)
		if !ok {
			t.Fatalf("no service %s", c.service)
		}
		blockLabels := []string{routeHttpNoLabel, routeHttpInProcessBlock}
		for _, block := range routeService.Blocks {
			blockLabels = append(blockLabels, block.Name)
		}
		countOf := func(blockLabel string) float64 {
			return testutil.ToFloat64(e2e.front.metrics.requests.WithLabelValues(c.service, c.protocol, "2xx", blockLabel))
		}
		beforeCounts := map[string]float64{}
		for _, blockLabel := range blockLabels {
			beforeCounts[blockLabel] = countOf(blockLabel)
		}
		requestCounts := e2e.requestCounts()

		request := newE2eRequest(t, "GET", "https://"+c.host+c.path, nil)
		var response *http.Response
		if c.protocol == "h2" {
			response, _ = e2eRoundTripH2(t, h2ClientConn, request)
		} else {
			response, _ = e2eDo(t, e2e.newH1Client(t, eth1.httpsAddr, c.host), request)
		}
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", what, response.StatusCode)
			continue
		}
		// the count is taken as the front's handler returns
		eth1.httpsServed.await(t, c.path)

		block := c.block
		if block == "" {
			requests := e2e.requestsSince(requestCounts)
			if len(requests) != 1 {
				t.Errorf("%s: %d blocks got it", what, len(requests))
				continue
			}
			block = requests[0].block
		}
		for _, blockLabel := range blockLabels {
			delta := countOf(blockLabel) - beforeCounts[blockLabel]
			want := 0.0
			if blockLabel == block {
				want = 1
			}
			if delta != want {
				t.Errorf("%s: the requests counted for block %q moved by %v, want %v (the request was answered by %s)", what, blockLabel, delta, want, block)
			}
		}
	}
}
