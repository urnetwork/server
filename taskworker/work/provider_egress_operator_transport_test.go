// The taskworker is the platform: its URL pass reaches the operator ingest
// in-process by default, and makes no operator API request at all.
package work

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/localoperator"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
)

// The transport is placement only: an unknown name is refused when settings
// load, the default and "local" are in-process, and "http" keeps the API path
// with its vault secret (absent from a test's fixture vault, so it fails before
// any shard is allocated).
func TestProviderEgressOperatorTransportSelection(t *testing.T) {
	settings := defaultProviderEgressProbeSettings("example.test")
	for _, transport := range []string{"", providerEgressOperatorLocal, providerEgressOperatorHttp} {
		settings.OperatorTransport = transport
		if err := settings.validate(); err != nil {
			t.Fatalf("operator_transport %q refused: %v", transport, err)
		}
	}
	settings.OperatorTransport = "grpc"
	if err := settings.validate(); err == nil {
		t.Fatal("an unknown operator_transport was accepted")
	}

	args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("example.test"), 1)
	for _, transport := range []string{"", providerEgressOperatorLocal} {
		operator, err := newProviderEgressOperatorForTransport(args, transport)
		if err != nil {
			t.Fatalf("operator_transport %q: %v", transport, err)
		}
		local, ok := operator.(*localoperator.Operator)
		if !ok {
			t.Fatalf("operator_transport %q is %T, not the in-process operator", transport, operator)
		}
		if local.ShardIndex != args.ShardIndex || local.ShardCount != args.ShardCount || local.CallTimeout != providerEgressControlPlaneTimeout {
			t.Fatalf("the in-process operator lost the shard or the request bound: %+v", local)
		}
	}
	if _, err := newProviderEgressOperatorForTransport(args, "grpc"); err == nil {
		t.Fatal("an unknown transport built an operator")
	}
	t.Setenv(server.TestRealVaultEnv, "")
	if _, err := newProviderEgressOperatorForTransport(args, providerEgressOperatorHttp); err == nil {
		t.Fatal("the HTTP transport built an operator without the vault's ingest secret")
	}
	// the pass keeps the arguments it was scheduled with: a transport change is
	// not a stale snapshot
	settings = defaultProviderEgressProbeSettings("example.test")
	before := providerEgressProbeArgs(settings, 0)
	settings.OperatorTransport = providerEgressOperatorHttp
	if !providerEgressProbeArgsMatchSettings(before, settings) {
		t.Fatal("changing operator_transport made the durable pass arguments stale")
	}
}

// A catalog this test owns, so the destination pool is built in-process from
// config rather than from a workstation's qualityprobe.yml.
const testOperatorTransportCatalog = `
schema_version: 1
destinations:
  - {name: transport-dns, class: dns, category: dns, url: "https://dns.example/dns-query?name=example.com&type=A", verify: {kind: dns_json}}
  - {name: transport-connectivity, class: connectivity, category: connectivity, url: "https://connectivity.example/", verify: {kind: contains, text: transport-success}}
  - {name: transport-cdn, class: cdn, category: cdn, url: "https://cdn.example/"}
  - {name: transport-news, class: site, category: news, url: "https://news.example/"}
settings:
  site_pool_size: {dns: 1, connectivity: 1, cdn: 1, site: 1}
  site_sample_size: {dns: 1, connectivity: 1, cdn: 1, site: 1}
candidates: []
`

// One provider due for its first URL turn: connected, placed and providing.
func testOperatorTransportDueProvider(tb testing.TB, ctx context.Context) server.Id {
	city := &model.Location{
		LocationType: model.LocationTypeCity,
		City:         "Palo Alto",
		Region:       "California",
		Country:      "United States",
		CountryCode:  "us",
	}
	model.CreateLocation(ctx, city)
	clientId := server.NewId()
	model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
	connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, "192.0.2.1:0", model.CreateNetworkClientHandler(ctx))
	if err != nil {
		tb.Fatalf("connect client: %s", err)
	}
	if err := model.SetConnectionLocation(ctx, connectionId, city.LocationId, &model.ConnectionLocationScores{}); err != nil {
		tb.Fatalf("set connection location: %s", err)
	}
	model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{model.ProvideModePublic: []byte("provide-secret")})
	model.UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())
	return clientId
}

// One measured URL success, as the real egress-health check reports it.
func testOperatorTransportUrlSuccess(measuredAt time.Time) *egresshealth.Result {
	destination := egresshealth.Destination{Name: "transport-news", Class: egresshealth.ClassSite, Url: "https://news.example/"}
	return &egresshealth.Result{
		OkCount: 1,
		Total:   1,
		ByClass: map[egresshealth.Class]egresshealth.ClassSummary{egresshealth.ClassSite: {Ok: 1, Total: 1}},
		Checks:  []egresshealth.CheckResult{{Name: destination.Name, Class: destination.Class, Ok: true, Attempts: 1}},
		UrlProbeEvidence: &egresshealth.UrlProbeEvidence{
			PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
			Destination: destination, MeasuredAt: measuredAt, ContentMatcherVersion: 1,
			Security:   []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: measuredAt, TlsAuthenticated: true}},
			StatusCode: 200, ByteCount: 32, WireByteCount: 32, WireSampleByteCount: 31, BodyComplete: true,
			RequestWritten: true, FirstByteReceived: true, TtfbMillis: 10, BodyMillis: 1, BodyBitsPerSecond: 248000,
			ContentClassification: "content", PerformanceClassification: "insufficient_sample",
		},
	}
}

// The production URL pass -- the operator runProviderEgressProbe selects by
// default and the pass newProviderUrlProbePass builds -- claims a provider,
// runs the real prober over it, publishes the measured outcome and completes
// the claim, all in the database, without one operator API request. Two traps
// make any request fail the test: the configured api_url is a listener that
// fails on contact, and every dial of the process's default HTTP transport
// fails. Only the provider tunnel and its one URL load are replaced, because
// the in-tunnel path is not an operator call and needs a real provider.
func TestProviderUrlProbePassMakesNoOperatorApiRequests(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var apiRequests atomic.Int64
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiRequests.Add(1)
			tb.Errorf("the URL pass requested the operator API: %s %s", r.Method, r.URL.Path)
			http.Error(w, "the operator API is closed to this test", http.StatusServiceUnavailable)
		}))
		defer trap.Close()
		var dials atomic.Int64
		defaultTransport := http.DefaultTransport
		trapped := defaultTransport.(*http.Transport).Clone()
		refuse := func(ctx context.Context, network string, address string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("the URL pass dialed through the default HTTP transport")
		}
		trapped.DialContext, trapped.DialTLSContext, trapped.Proxy = refuse, refuse, nil
		http.DefaultTransport = trapped
		defer func() { http.DefaultTransport = defaultTransport }()

		defer server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(testOperatorTransportCatalog))()
		// The in-process path needs no operator secret, and nothing on it may
		// gate on one: run it with provider_egress.yml present but without its
		// ingest_secret, the state an http rollback would refuse.
		defer server.Vault.PushSimpleResource("provider_egress.yml", []byte("note: no ingest secret\n"))()
		if _, err := readProviderEgressOperatorSecret(); err == nil {
			tb.Fatal("the test vault still holds an operator ingest secret")
		}
		providerId := testOperatorTransportDueProvider(tb, ctx)
		proberNetworkId := server.NewId()
		server.Raise(model.AddBasicTransferBalance(ctx, proberNetworkId, model.ByteCount(1024*1024*1024), server.NowUtc().Add(-time.Minute), server.NowUtc().Add(time.Hour)))

		settings := defaultProviderEgressProbeSettings("example.test")
		args := providerEgressProbeArgs(settings, 0)
		args.ShardCount = 1
		args.APIURL, args.PublicAPIURL = trap.URL, trap.URL
		args.PlatformURL = "wss://" + strings.TrimPrefix(trap.URL, "http://")
		args.UrlProbe.Limit, args.UrlProbe.Concurrency = 1, 1

		operator, err := newProviderEgressOperator(args, settings.OperatorTransport)
		if err != nil {
			tb.Fatalf("default operator transport: %v", err)
		}
		if _, ok := operator.(*localoperator.Operator); !ok {
			tb.Fatalf("the default operator transport is %T, not in-process", operator)
		}
		pass := newProviderUrlProbePass(args, operator, providertunnel.Config{
			ApiUrl: args.APIURL, PlatformUrl: args.PlatformURL,
		}, proberNetworkId)

		var opened atomic.Int64
		pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			if !options.UrlProbe || options.TunnelConfig.ApiUrl != trap.URL {
				tb.Error("the pass changed the URL turn or its control-plane endpoint")
			}
			p := &prober.Prober{
				Open: func(context.Context, string) (*http.Client, func() error, error) {
					opened.Add(1)
					return &http.Client{Transport: trapped}, func() error { return nil }, nil
				},
				Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
					return testOperatorTransportUrlSuccess(time.Now().UTC()), nil
				},
				Submit: options.Submit, Attempts: options.Attempts, HealthResults: options.HealthResults,
			}
			return (&prober.Scheduler{Prober: p, Concurrency: 1}).Run(ctx, providers), nil
		}

		result, err := pass.run(ctx, args)
		if err != nil {
			tb.Fatalf("URL pass: %v", err)
		}
		if result.UrlDue != 1 || result.Attempted != 1 || result.Submitted != 1 || result.Failed != 0 || opened.Load() != 1 {
			tb.Fatalf("the pass did not claim, measure and accept exactly the one due provider: %+v opened=%d", result, opened.Load())
		}
		if n := apiRequests.Load(); n != 0 {
			tb.Fatalf("the URL pass made %d operator API requests", n)
		}
		if n := dials.Load(); n != 0 {
			tb.Fatalf("the URL pass dialed %d times through the default HTTP transport", n)
		}

		// The durable effects are the ingest's own: the claim is completed and
		// the measured outcome is in the provider's accepted history.
		var completed, accepted int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_url_probe_run
				WHERE client_id=$1 AND completed_at IS NOT NULL AND probe_failure=''`, providerId).Scan(&completed))
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_egress_health_history
				WHERE client_id=$1 AND total_count=1 AND ok_count=1 AND url_probe_policy_version=$2`,
				providerId, egresshealth.UrlProbePolicyVersion).Scan(&accepted))
		})
		if completed != 1 || accepted != 1 {
			tb.Fatalf("in-process publication left completed=%d accepted=%d, want 1 and 1", completed, accepted)
		}
	})
}

// The HTTP transport stays available for rollback and for the standalone
// prober: the same pass constructor over an ingest.Client sends its operator
// calls to the configured api_url, here a listener that records and refuses
// them, and never touches the database ingest.
func TestProviderUrlProbePassHttpTransportUsesTheOperatorApi(t *testing.T) {
	var requests atomic.Int64
	paths := make(chan string, 16)
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case paths <- r.URL.Path:
		default:
		}
		http.Error(w, "refused", http.StatusServiceUnavailable)
	}))
	defer trap.Close()
	args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("example.test"), 0)
	args.APIURL = trap.URL
	operator := &ingest.Client{ServerUrl: trap.URL, OperatorSecret: "synthetic", ShardIndex: args.ShardIndex, ShardCount: args.ShardCount, Http: trap.Client()}
	pass := newProviderUrlProbePass(args, operator, providertunnel.Config{ApiUrl: trap.URL}, server.NewId())
	pass.readiness = nil
	pass.refreshFleet = nil
	pass.runFull = func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
		t.Fatal("a refused catalog opened a tunnel")
		return prober.Summary{}, nil
	}
	if _, err := pass.run(context.Background(), args); !errors.Is(err, egresshealth.ErrPoolUnavailable) {
		t.Fatalf("a refused pool fetch was not the pass error: %v", err)
	}
	if requests.Load() != 1 || <-paths != egresshealth.PoolPath {
		t.Fatalf("the HTTP transport did not fetch the pool from the operator API: requests=%d", requests.Load())
	}
}
