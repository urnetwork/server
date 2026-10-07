package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A refusing API is distinct from the fixture provider's control origin and
// the payload origin. Actual hosted mint/renew/refresh/reconnect must still
// deliver traffic and settle the caller's ledger without touching that API.
func TestProxyCriticalControlUsesLocalAuthorityWithoutHttp(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		var requests atomic.Int64
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			http.Error(w, "control API disabled", http.StatusServiceUnavailable)
		}))
		defer trap.Close()
		opts := defaultProxyTestOptions()
		opts.hostedApiUrl = trap.URL
		opts.disableSecurityPolicies = true
		restore := withSmallContracts()
		defer restore()
		h := setupProxyTestWithOptions(t, opts)
		defer h.close(t)
		pd, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(h.ctx, 90*time.Second)
		defer cancel()
		refreshed, err := pd.deviceLocal.GetApi().RefreshJwtSyncWithContext(ctx)
		if err != nil || refreshed == nil || refreshed.ByJwt == "" {
			t.Fatal("local refresh failed", err)
		}
		balanceDone := make(chan error, 1)
		pd.deviceLocal.GetApi().SubscriptionBalance(connect.NewApiCallback[*sdk.SubscriptionBalanceResult](func(result *sdk.SubscriptionBalanceResult, err error) {
			if err == nil && result == nil {
				err = fmt.Errorf("missing balance result")
			}
			balanceDone <- err
		}))
		select {
		case err := <-balanceDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		locationsDone := make(chan error, 1)
		pd.deviceLocal.GetApi().GetProviderLocations(connect.NewApiCallback[*sdk.FindLocationsResult](func(result *sdk.FindLocationsResult, err error) {
			if err == nil && result == nil {
				err = fmt.Errorf("missing locations result")
			}
			locationsDone <- err
		}))
		select {
		case err := <-locationsDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		target, closeTarget := startLocalTarget(t)
		defer closeTarget()
		client := newSocksProxyClient(t, h.signedProxyId, h.socksPort, 40*time.Second)
		defer client.CloseIdleConnections()
		// A fixed-only window bypasses discovery. Mix a real BestAvailable
		// request with the known payload peer so a lost SDK discovery hook
		// reaches the trap even when its fixed-destination fallback works.
		specs := sdk.NewProviderSpecList()
		specs.Add(&sdk.ProviderSpec{ClientId: server.ToSdkId(h.providerClientId)})
		specs.Add(&sdk.ProviderSpec{BestAvailable: true})
		pd.deviceLocal.SetDestination(pd.deviceLocal.GetConnectLocation(), specs)
		if !pd.WaitForReady(ctx, 30*time.Second) {
			t.Fatal("discovery window did not become ready")
		}
		if err := fetchData(client, target, 8*testMib); err != nil {
			t.Fatal("first transfer", err)
		}
		pd.deviceLocal.Reconnect(pd.deviceLocal.GetConnectLocation())
		if !pd.WaitForReady(ctx, 30*time.Second) {
			t.Fatal("reconnect did not become ready")
		}
		if err := fetchData(client, target, 8*testMib); err != nil {
			t.Fatal("renewed transfer", err)
		}
		if total, _ := escrowedContractCounts(ctx, h.pdNetworkId); total < 4 {
			t.Fatalf("renewal did not create enough real escrows: %d", total)
		}
		client.CloseIdleConnections()
		if err := h.proxyDeviceManager.CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
		h.closeProvider(t)
		settleAllEscrowedContracts(t, ctx, h.pdNetworkId)
		for shard := range model.TransferDebitShardCount {
			if _, err := model.FlushTransferDebits(ctx, shard, nil, 64); err != nil {
				t.Fatal("asynchronous payer debit", err)
			}
		}
		payout := settledPayoutSum(ctx, h.pdNetworkId)
		if payout < 16*testMib || pgActiveBalanceSum(ctx, h.pdNetworkId) != opts.pdInitialBalance-payout {
			t.Fatal("actual transferred usage did not reconcile against the hosted payer")
		}
		if n := requests.Load(); n != 0 {
			t.Fatalf("hosted critical path made %d HTTP API requests", n)
		}
	})
}

// Ablate each optional generator boundary separately. Discovery deliberately
// uses BestAvailable (fixed client ids bypass discovery). Positive operations
// use the real local authority; each missing hook must trip the HTTP trap.
func TestProxyLocalHooksHaveIndependentHttpRedControls(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		var requests atomic.Int64
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			http.Error(w, "API disabled", http.StatusServiceUnavailable)
		}))
		defer trap.Close()
		opts := defaultProxyTestOptions()
		opts.hostedApiUrl = trap.URL
		opts.controlPlaneOnly = true
		h := setupProxyTestWithOptions(t, opts)
		defer h.close(t)
		pd, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(h.ctx, 20*time.Second)
		defer cancel()
		strategySettings := connect.DefaultClientStrategySettings()
		strategySettings.EnableResilient = false
		strategySettings.RequestTimeout = time.Second
		strategy := connect.NewClientStrategy(ctx, strategySettings)
		defer strategy.Close()
		parent := connect.Id(h.pdClientId)
		generator := func(credentials, discovery bool) *connect.ApiMultiClientGenerator {
			s := connect.DefaultApiMultiClientGeneratorSettings()
			if credentials {
				s.ClientCredentials = pd.localControl
			}
			if discovery {
				s.ProviderDiscovery = pd.localControl
			}
			return connect.NewApiMultiClientGenerator(ctx, []*connect.ProviderSpec{{BestAvailable: true}}, strategy, nil, trap.URL, h.pdByClientJwt, h.platformUrl, "test", "test", "test", &parent, connect.DefaultClientSettings, s)
		}
		for _, kind := range []string{"credentials", "discovery"} {
			for _, enabled := range []bool{true, false} {
				before := requests.Load()
				g := generator(kind != "credentials" || enabled, kind != "discovery" || enabled)
				var callErr error
				if kind == "credentials" {
					args, e := g.NewClientArgsContext(ctx)
					callErr = e
					if e == nil {
						g.RemoveClientArgs(args)
					}
				} else {
					_, callErr = g.NextDestinationsContext(ctx, 2, nil, "quality")
				}
				if err := g.CloseAndWait(ctx); err != nil {
					t.Fatal(err)
				}
				if enabled && (callErr != nil || requests.Load() != before) {
					t.Fatalf("local %s failed or used HTTP: %v", kind, callErr)
				}
				if !enabled && (callErr == nil || requests.Load() == before) {
					t.Fatalf("missing %s hook did not expose HTTP dependency", kind)
				}
			}
		}
		child, err := pd.localControl.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &parent, Description: "control-test", DeviceSpec: "test"})
		if err != nil || child == nil || child.Error != nil {
			t.Fatal("real child mint failed", err)
		}
		claims, err := connect.ParseByJwtUnverified(child.ByClientJwt)
		if err != nil {
			t.Fatal(err)
		}
		for _, enabled := range []bool{true, false} {
			before := requests.Load()
			var local connect.NetworkClientControl
			if enabled {
				local = pd.localControl
			}
			var oob *connect.ApiOutOfBandControl
			if local != nil {
				oob = connect.NewApiOutOfBandControlWithLocalControl(ctx, strategy, child.ByClientJwt, trap.URL, local)
			} else {
				oob = connect.NewApiOutOfBandControl(ctx, strategy, child.ByClientJwt, trap.URL)
			}
			frame, e := connect.ToFrame(&protocol.Provide{Keys: []*protocol.ProvideKey{{Mode: protocol.ProvideMode_Network, ProvideSecretKey: make([]byte, 32)}}}, connect.DefaultProtocolVersion)
			if e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			oob.SendControlWithCtx(ctx, []*protocol.Frame{frame}, func(_ []*protocol.Frame, e error) { done <- e })
			var callErr error
			select {
			case callErr = <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := oob.CloseAndWait(ctx); err != nil {
				t.Fatal(err)
			}
			if enabled && (callErr != nil || requests.Load() != before) {
				t.Fatal("local control failed or used HTTP", callErr)
			}
			if !enabled && (callErr == nil || requests.Load() == before) {
				t.Fatal("missing control hook did not expose HTTP dependency")
			}
		}
		modes, err := model.GetProvideModes(ctx, server.Id(claims.ClientId))
		if err != nil || !modes[model.ProvideModeNetwork] {
			t.Fatal("positive control did not perform real registration", err)
		}
		if _, err = pd.localControl.RemoveNetworkClient(ctx, &connect.RemoveNetworkClientArgs{ClientId: claims.ClientId}); err != nil {
			t.Fatal(err)
		}
		// Join asynchronous refresh, removals and data owners before the final
		// stable count. Only the three deliberate negative requests may be HTTP.
		before := requests.Load()
		if err = h.proxyDeviceManager.CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
		if requests.Load() != before {
			t.Fatal("retirement unexpectedly used the HTTP API")
		}
	})
}
