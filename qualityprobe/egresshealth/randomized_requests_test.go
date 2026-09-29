// A probe begins with its random sample, never an operator fingerprint.
package egresshealth

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Records every application request and returns a valid small website body.
type sampledRequestTransport struct {
	stateLock sync.Mutex
	paths     []string
}

// A country with many incompatible sites does not add an unbounded fixed
// control set. Successive deterministic seeds vary the bounded unscored sample.
func TestPlaceCanariesAreBoundedRandomSamples(t *testing.T) {
	table := []Destination{{Name: "ordinary", Class: ClassSite, Url: "https://sample.example/ordinary"}}
	for i := range 12 {
		table = append(table, Destination{Name: fmt.Sprintf("canary-%d", i), Class: ClassSite, Url: fmt.Sprintf("https://sample.example/canary-%d", i), Canary: true, Incompatible: []Place{{Country: "de"}}})
	}
	var previous []string
	for _, seed := range []int64{31, 73} {
		transport := &sampledRequestTransport{}
		opts := fastOptions()
		opts.Concurrency = 1
		opts.Destinations = table
		opts.ProviderPlace = Place{Country: "de"}
		opts.Rand = rand.New(rand.NewSource(seed))
		res, err := Check(context.Background(), &http.Client{Transport: transport}, opts)
		if err != nil || res.Total != 1 || len(res.Checks) != 3 || len(res.CanaryPassedNames()) != 2 || len(transport.paths) != 3 {
			t.Fatalf("unbounded or scored canaries: result=%+v requests=%v err=%v", res, transport.paths, err)
		}
		selected := res.CanaryPassedNames()
		if previous != nil && slices.Equal(previous, selected) {
			t.Fatalf("successive seeds retained a fixed canary set: %v", selected)
		}
		previous = selected
	}
}

// Implements the client seam without any network dependency.
func (self *sampledRequestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	self.stateLock.Lock()
	self.paths = append(self.paths, req.URL.Path)
	self.stateLock.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("User-agent: *")), Request: req}, nil
}

// Even a retained legacy echo setting must not add a predictable request.
func TestFullRequestsOnlySampledUrls(t *testing.T) {
	transport := &sampledRequestTransport{}
	opts := fastOptions()
	opts.IpEchoUrl = "https://operator.example/my-ip-info"
	res, err := check(context.Background(), &http.Client{Transport: transport}, []Destination{{Name: "sample", Class: ClassSite, Url: "https://sample.example/robots.txt"}}, opts)
	if err != nil || res.OkCount != 1 || res.Total != 1 {
		t.Fatalf("sample did not complete: result=%+v err=%v", res, err)
	}
	if len(transport.paths) != 1 || transport.paths[0] != "/robots.txt" {
		t.Fatalf("non-sampled application request: %v", transport.paths)
	}
	if res.ExitIp != "" || !res.ExitObservedAt.IsZero() || res.IpEchoErr != "" {
		t.Fatal("a website response invented exit-location evidence")
	}
}

// The cheap check has the same no-fingerprint boundary as Full.
func TestBlackholeRequestsOnlySampledUrls(t *testing.T) {
	transport := &sampledRequestTransport{}
	opts := fastOptions()
	opts.IpEchoUrl = "https://operator.example/my-ip-info"
	res := blackhole(context.Background(), &http.Client{Transport: transport}, []Destination{{Name: "sample", Class: ClassConnectivity, Url: "https://sample.example/robots.txt"}}, opts)
	if !res.Ok || len(transport.paths) != 1 || transport.paths[0] != "/robots.txt" {
		t.Fatalf("unexpected cheap requests: result=%+v paths=%v", res, transport.paths)
	}
	if res.ExitIp != "" || res.IpEchoErr != "" {
		t.Fatal("a cheap website response invented exit evidence")
	}
}
