package fleetprobe

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
)

// Tests of the full pass: the pool and profile a health run draws from, the
// options geometry and validation, and Open's failures.

// An http.RoundTripper that answers every request itself and records the
// host and user agent each was sent with.
type recordingRoundTripper struct {
	stateLock sync.Mutex
	hosts     []string
	agents    []string
}

// Implements http.RoundTripper: the echo answers with an address, every
// other request with a body.
func (self *recordingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.hosts = append(self.hosts, request.URL.Host)
		self.agents = append(self.agents, request.Header.Get("User-Agent"))
	}()
	body := "ok"
	if request.URL.Path == egresshealth.IpEchoPath {
		body = `{"info":{"ip":"198.51.100.4"}}`
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: request}, nil
}

// Reports whether any request went to host.
func (self *recordingRoundTripper) requested(host string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, h := range self.hosts {
		if h == host {
			return true
		}
	}
	return false
}

// One site per class plus one site that does not work from
// Germany, carrying its own browser profile.
func smallPool() *egresshealth.Pool {
	return &egresshealth.Pool{
		Version: 7,
		Destinations: []egresshealth.Destination{
			{Name: "dns", Class: egresshealth.ClassDns, Url: "https://dns.pool.example/q", Verify: egresshealth.BodyCheck{Kind: egresshealth.BodyCheckContains, Text: "ok"}},
			{Name: "conn", Class: egresshealth.ClassConnectivity, Url: "https://conn.pool.example/", Verify: egresshealth.BodyCheck{Kind: egresshealth.BodyCheckContains, Text: "ok"}},
			{Name: "cdn", Class: egresshealth.ClassCdn, Url: "https://cdn.pool.example/"},
			{Name: "site", Class: egresshealth.ClassSite, Url: "https://site.pool.example/"},
			{Name: "not-in-de", Class: egresshealth.ClassSite, Url: "https://not-in-de.pool.example/", Incompatible: []egresshealth.Place{{Country: "de"}}},
		},
		Profile: egresshealth.RequestProfile{UserAgent: "Mozilla/5.0 pool-profile"},
	}
}

// The health hook
// draws from the pass's pool, shaped with the pool's profile, for the place
// the due list gave the provider, and warms up on the echo derived from the
// api url first.
func TestFullProberHealthRunsThePassPoolForTheProvidersPlace(t *testing.T) {
	transport := &recordingRoundTripper{}
	providerProber := NewFullProber(FullOptions{
		TunnelConfig: providertunnel.Config{ApiUrl: "https://api.operator.example/"},
		ProbeTimeout: time.Minute,
		Pool:         func() *egresshealth.Pool { return smallPool() },
		LoadAttempts: 1,
	})
	res, err := providerProber.Health(context.Background(), &http.Client{Transport: transport}, egresshealth.Place{Country: "de"})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if res.ExitIp != "198.51.100.4" {
		t.Errorf("ExitIp = %q, want the echo's answer", res.ExitIp)
	}
	if transport.hosts[0] != "api.operator.example" {
		t.Errorf("first request went to %q, want the warm-up on the operator's echo", transport.hosts[0])
	}
	for _, host := range []string{"dns.pool.example", "conn.pool.example", "cdn.pool.example", "site.pool.example"} {
		if !transport.requested(host) {
			t.Errorf("pool destination %s was not loaded", host)
		}
	}
	if transport.requested("not-in-de.pool.example") {
		t.Error("a destination incompatible with the provider's country was loaded")
	}
	for i, agent := range transport.agents {
		if agent != "Mozilla/5.0 pool-profile" {
			t.Errorf("request %d carried agent %q, want the pool's profile", i, agent)
		}
	}
	if res.TableTotal != len(smallPool().Destinations) {
		t.Errorf("TableTotal = %d, want the pool's %d", res.TableTotal, len(smallPool().Destinations))
	}
}

// No pool source is the built-in
// table, never an empty run.
func TestFullProberFallsBackToTheBuiltinTable(t *testing.T) {
	transport := &recordingRoundTripper{}
	providerProber := NewFullProber(FullOptions{
		TunnelConfig: providertunnel.Config{ApiUrl: "https://api.operator.example"},
		ProbeTimeout: time.Minute,
		LoadAttempts: 1,
	})
	res, err := providerProber.Health(context.Background(), &http.Client{Transport: transport}, egresshealth.Place{})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if res.TableTotal != len(egresshealth.Destinations()) || res.Total+res.NotMeasured != egresshealth.SamplePerRun() {
		t.Fatalf("TableTotal = %d, loads = %d, want the built-in table's %d and its %d-load sample", res.TableTotal, res.Total, len(egresshealth.Destinations()), egresshealth.SamplePerRun())
	}
}

// The probe timeout is the cold-start
// allowance -- the warm-up's own timeout -- and each load attempt gets the
// smaller of it and the per-request floor. The budget is left derived, since a
// run spans its retry schedule now.
func TestEgressHealthOptionsGeometry(t *testing.T) {
	opts := EgressHealthOptions(60*time.Second, false)
	if opts.IpEchoTimeout != 60*time.Second || opts.PerRequestTimeout != egresshealth.DefaultPerRequestTimeout {
		t.Errorf("60s: echo %s per-request %s", opts.IpEchoTimeout, opts.PerRequestTimeout)
	}
	if opts.Budget != 0 || opts.Concurrency != egresshealth.DefaultConcurrency || opts.AllDestinations {
		t.Errorf("60s: budget %s concurrency %d all %t", opts.Budget, opts.Concurrency, opts.AllDestinations)
	}
	if short := EgressHealthOptions(5*time.Second, false); short.PerRequestTimeout != 5*time.Second || egresshealth.DefaultPerRequestTimeout <= short.PerRequestTimeout {
		t.Errorf("5s: per-request %s; a probe timeout under the floor must show under it so startup can refuse it", short.PerRequestTimeout)
	}
	if all := EgressHealthOptions(time.Minute, true); all.Concurrency != egresshealth.AllConcurrency || !all.AllDestinations {
		t.Errorf("all: %+v", all)
	}
}

// A pass needs a probe timeout, the reporters, and an
// /ip echo to take the exit from; pins are optional and zero concurrency is
// the default.
func TestValidateFullOptions(t *testing.T) {
	good := FullOptions{
		TunnelConfig: providertunnel.Config{ApiUrl: "https://api.operator.example"},
		ProbeTimeout: time.Minute,
		Submit:       nopSubmitter{},
		Attempts:     nopAttempts{},
	}
	if err := validateFullOptions(good); err != nil {
		t.Fatalf("valid options refused: %v", err)
	}
	if got := good.concurrency(); got != DefaultFullConcurrency || DefaultFullConcurrency != 16 {
		t.Errorf("zero concurrency = %d, want the default 16", got)
	}
	if got := good.ipEchoUrl(); got != "https://api.operator.example"+egresshealth.IpEchoPath {
		t.Errorf("echo url = %q", got)
	}
	for name, mutate := range map[string]func(*FullOptions){
		"no echo":              func(o *FullOptions) { o.TunnelConfig.ApiUrl = "" },
		"no timeout":           func(o *FullOptions) { o.ProbeTimeout = 0 },
		"negative concurrency": func(o *FullOptions) { o.Concurrency = -1 },
		"no submitter":         func(o *FullOptions) { o.Submit = nil },
		"no attempts":          func(o *FullOptions) { o.Attempts = nil },
	} {
		options := good
		mutate(&options)
		if err := validateFullOptions(options); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	explicit := good
	explicit.TunnelConfig.ApiUrl = ""
	explicit.IpEchoUrl = "https://public.operator.example/my-ip-info"
	if err := validateFullOptions(explicit); err != nil || explicit.ipEchoUrl() != "https://public.operator.example/my-ip-info" {
		t.Errorf("an explicit echo url was not used: %v", err)
	}
}

// A prober.Submitter that accepts everything.
type nopSubmitter struct{}

// Implements prober.Submitter.
func (nopSubmitter) Submit(context.Context, string, string, time.Time) error { return nil }

// A prober.AttemptReporter that accepts everything.
type nopAttempts struct{}

// Implements prober.AttemptReporter.
func (nopAttempts) ReportAttempt(context.Context, string, string) error { return nil }

// A provider id that does not parse
// fails at Open, before any tunnel.
func TestFullProberOpenFailureIsATunnelError(t *testing.T) {
	providerProber := NewFullProber(FullOptions{TunnelConfig: providertunnel.Config{ApiUrl: "https://api.operator.example"}, ProbeTimeout: time.Minute})
	if _, _, err := providerProber.Open(context.Background(), "not-an-id"); err == nil {
		t.Fatal("Open accepted a provider id that does not parse")
	}
}
