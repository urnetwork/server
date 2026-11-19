package nginx

// End-to-end tests of what the assembled front records (D17).

import (
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// D17: the front counts each request by the service it is for, its
// protocol, its status class and the block that answered. A balanced
// forwarded request names the block the balancer chose, and one retried
// after a 503 names the block that answered the retry, never the one that
// refused it (D7). A per-block lb route names its block, and connect's
// in-process handler "local". No other label of the service moves, in the
// request's status class or in 5xx.
func TestFrontE2eCountsRequestsByTheBlockThatAnswered(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	h2ClientConn, _ := e2e.newH2ClientConn(t, eth1.httpsAddr, "api.service.example")
	cases := []struct {
		protocol string
		host     string
		path     string
		service  string
		// the block label the count names; empty for the block that
		// answered
		block string
		// what each attempt across the service's blocks answers, in order;
		// nil for one attempt, answered by its block's default 200
		attemptStatuses []int
	}{
		{protocol: "h1", host: "web.service.example", path: "/counted-web", service: "web"},
		{protocol: "h1", host: "api.service.example", path: "/counted-api", service: "api"},
		{protocol: "h2", host: "api.service.example", path: "/counted-api-h2", service: "api"},
		{protocol: "h1", host: "api.service.example", path: "/counted-api-retried", service: "api", attemptStatuses: []int{http.StatusServiceUnavailable, http.StatusOK}},
		{protocol: "h2", host: "api.service.example", path: "/counted-api-retried-h2", service: "api", attemptStatuses: []int{http.StatusServiceUnavailable, http.StatusOK}},
		{protocol: "h1", host: "test-lb.service.example", path: "/hidden-example/by/b/api/g2/counted-block", service: "api", block: "g2"},
		{protocol: "h1", host: "connect.service.example", path: "/?counted-connect", service: "connect", block: routeHttpInProcessBlock},
	}
	statusClasses := []string{"2xx", "5xx"}
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
		countOf := func(statusClass string, blockLabel string) float64 {
			return testutil.ToFloat64(e2e.front.metrics.requests.WithLabelValues(c.service, c.protocol, statusClass, blockLabel))
		}
		beforeCounts := map[string]float64{}
		for _, statusClass := range statusClasses {
			for _, blockLabel := range blockLabels {
				beforeCounts[statusClass+" "+blockLabel] = countOf(statusClass, blockLabel)
			}
		}
		requestCounts := e2e.requestCounts()
		attempts := &e2eAttempts{}
		if c.attemptStatuses != nil {
			e2e.setServiceHandler(t, c.service, attempts.handler(func(attemptNumber int) int {
				return c.attemptStatuses[min(attemptNumber, len(c.attemptStatuses))-1]
			}))
		}

		request := newE2eRequest(t, "GET", "https://"+c.host+c.path, nil)
		var response *http.Response
		if c.protocol == "h2" {
			response, _ = e2eRoundTripH2(t, h2ClientConn, request)
		} else {
			response, _ = e2eDo(t, e2e.newH1Client(t, eth1.httpsAddr, c.host), request)
		}
		if c.attemptStatuses != nil {
			e2e.setServiceHandler(t, c.service, nil)
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
			attemptCount := max(len(c.attemptStatuses), 1)
			if len(requests) != attemptCount {
				t.Errorf("%s: %d attempts reached the blocks, want %d", what, len(requests), attemptCount)
				continue
			}
			block = requests[0].block
			if c.attemptStatuses != nil {
				// the last attempt answered
				recorded := attempts.recorded()
				block = recorded[len(recorded)-1].block
				if recorded[0].block == block {
					t.Errorf("%s: both attempts went to %s, want the retry on the other block", what, block)
					continue
				}
			}
		}
		for _, statusClass := range statusClasses {
			for _, blockLabel := range blockLabels {
				delta := countOf(statusClass, blockLabel) - beforeCounts[statusClass+" "+blockLabel]
				want := 0.0
				if statusClass == "2xx" && blockLabel == block {
					want = 1
				}
				if delta != want {
					t.Errorf("%s: the %s requests counted for block %q moved by %v, want %v (the request was answered by %s)", what, statusClass, blockLabel, delta, want, block)
				}
			}
		}
	}
}
