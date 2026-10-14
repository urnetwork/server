package providertunnel

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/localclient"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
	"github.com/urnetwork/server/v2026/session"
)

// The real prober must never publish the unavailable instrument's result.
type localWriterPublicationTrap struct{ calls atomic.Int32 }

func (self *localWriterPublicationTrap) SubmitEgressHealth(context.Context, string, *egresshealth.Result) error {
	self.calls.Add(1)
	return nil
}

// Real local registration and financial allocation succeed, but the platform
// never admits a transport. The actual URL checker must not score its own
// unavailable writer as a negative provider measurement.
func TestUrlProbeRejectedLocalWriterIsNotMeasured(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		var apiCalls, platformCalls, dnsCalls atomic.Int64
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiCalls.Add(1)
			http.Error(w, "control API disabled", http.StatusServiceUnavailable)
		}))
		defer trap.Close()
		platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			platformCalls.Add(1)
			http.Error(w, "platform route unavailable", http.StatusServiceUnavailable)
		}))
		defer platform.Close()
		dns := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			dnsCalls.Add(1)
			http.Error(w, "unexpected direct resolver contact", http.StatusServiceUnavailable)
		}))
		defer dns.Close()
		sourceNetwork, sourceUser, sourceDevice, sourceClient := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		providerNetwork, providerClient := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, sourceNetwork, "local-writer-source", sourceUser)
		model.Testing_CreateDevice(ctx, sourceNetwork, sourceDevice, sourceClient, "source", "fixture")
		model.Testing_CreateNetwork(ctx, providerNetwork, "local-writer-provider", server.NewId())
		model.Testing_CreateDevice(ctx, providerNetwork, server.NewId(), providerClient, "provider", "fixture")
		model.SetProvide(ctx, providerClient, map[model.ProvideMode][]byte{model.ProvideModePublic: bytes.Repeat([]byte{42}, 32)})
		server.Raise(model.AddBasicTransferBalance(ctx, sourceNetwork, 1024*1024*1024, server.NowUtc(), server.NowUtc().Add(time.Hour)))
		token := session.NewByJwt(sourceNetwork, sourceUser, "local-writer-source", false, false).Client(sourceDevice, sourceClient).Testing_Sign()
		authority, err := localclient.New(ctx, token, trap.URL)
		if err != nil {
			tb.Fatal(err)
		}
		defer authority.Close()
		counted := &countedProbeAuthority{Authority: authority}
		tunnel, err := Open(ctx, Config{
			ApiUrl: trap.URL, PlatformUrl: "ws" + platform.URL[4:], ByJwt: token, ClientId: connect.Id(sourceClient),
			DataOnlyProbe: true, ClientCredentials: counted, ClientControl: counted, CloseTimeout: 10 * time.Second,
			DnsResolverSettings:          &connect.DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{dns.URL + "/dns-query"}},
			ContractReservationByteCount: 1024 * 1024,
		}, connect.Id(providerClient))
		if err != nil {
			tb.Fatal(err)
		}
		defer tunnel.Close()
		const target = "local-writer-payload.example"
		client := tunnel.HttpClientForHosts(15*time.Second, []string{target})
		defer client.CloseIdleConnections()
		var result *egresshealth.Result
		publication := &localWriterPublicationTrap{}
		probe := &prober.Prober{
			Open:          func(context.Context, string) (*http.Client, func() error, error) { return client, tunnel.Close, nil },
			HealthResults: publication,
			Health: func(checkCtx context.Context, checkClient *http.Client, _ egresshealth.Place) (*egresshealth.Result, error) {
				var checkErr error
				result, checkErr = egresshealth.Check(checkCtx, checkClient, egresshealth.Options{
					UrlProbe: true, PerRequestTimeout: 15 * time.Second, Budget: 20 * time.Second,
					Destinations: []egresshealth.Destination{{Name: "local-writer", Url: "https://" + target + "/payload", Class: egresshealth.ClassSite}},
				})
				return result, checkErr
			},
		}
		probeErr := probe.ProbeOne(ctx, prober.Provider{ClientId: providerClient.String()})
		if result == nil {
			tb.Fatal("actual checker did not return a result", probeErr)
		}

		var contracts int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE payer_network_id=$1`, sourceNetwork).Scan(&contracts))
		})
		registered := !client.Transport.(*providerHttpTransport).ProviderMeasurementUnavailable()
		contractUnavailable := tunnel.multiClient.ProviderContractAcquisitionUnavailable()
		closeErr := tunnel.Close()
		if closeErr != nil {
			tb.Fatal(closeErr)
		}
		if counted.mint.Load() == 0 || counted.control.Load() == 0 || counted.retire.Load() == 0 || platformCalls.Load() == 0 || contracts == 0 || !registered {
			tb.Fatalf("missing actual boundary: mint=%d control=%d retire=%d platform=%d contracts=%d registered=%t", counted.mint.Load(), counted.control.Load(), counted.retire.Load(), platformCalls.Load(), contracts, registered)
		}
		if apiCalls.Load() != 0 || dnsCalls.Load() != 0 {
			tb.Fatalf("unexpected API/resolver traffic: api=%d dns=%d", apiCalls.Load(), dnsCalls.Load())
		}
		tb.Logf("actual local allocation and rejected transport: contracts=%d control=%d platform=%d contract_unavailable=%t total=%d not_measured=%d stages=%s publications=%d", contracts, counted.control.Load(), platformCalls.Load(), contractUnavailable, result.Total, result.NotMeasured, result.FailureStageSummary(), publication.calls.Load())
		if result.Total != 0 || result.NotMeasured != 1 || result.FailureStageSummary() != "local_transport_admission:1" {
			tb.Fatalf("unavailable local writer created provider quota: total=%d not_measured=%d stages=%s", result.Total, result.NotMeasured, result.FailureStageSummary())
		}
		if !errors.Is(probeErr, prober.ErrNotMeasured) || publication.calls.Load() != 0 {
			tb.Fatalf("unavailable writer was accepted/published: err=%v publications=%d", probeErr, publication.calls.Load())
		}
	})
}
