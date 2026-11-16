// A probe begins with its random sample, never an operator fingerprint.
package egresshealth

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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
	for _, seed := range []uint64{31, 73} {
		transport := &sampledRequestTransport{}
		opts := fastOptions()
		opts.Concurrency = 1
		opts.Destinations = table
		opts.ProviderPlace = Place{Country: "de"}
		opts.Rand = rand.New(rand.NewPCG(seed, 0))
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

// A run with no injected generator draws its sample and spacing from a fresh
// connect.NewSecureRand generator (RANDUPDATE.md S1), so a provider cannot
// predict which destinations a pass asks it for. It was a Go 1 source, whose
// outputs give its state away, behind a clock seed for a crypto/rand read that
// cannot fail. Two runs' generators, built the production way under one fixed
// fake clock, draw different streams; an injected generator is used as given.
func TestRunGeneratorIsSecureAndPerRun(t *testing.T) {
	now := time.Date(2026, time.November, 13, 12, 0, 0, 0, time.UTC)
	opts := Options{Now: func() time.Time { return now }}
	draws := func(random *rand.Rand) []uint64 {
		values := []uint64{}
		for range 4 {
			values = append(values, random.Uint64())
		}
		return values
	}
	if first, second := draws(opts.rng()), draws(opts.rng()); slices.Equal(first, second) {
		t.Fatalf("two runs drew the same stream %v", first)
	}
	injected := rand.New(rand.NewPCG(1, 2))
	opts.Rand = injected
	if opts.rng() != injected {
		t.Fatal("an injected generator was not used as given")
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
