// A local platform with one real provider on it, for capability measurement
// tests through the real Open path.
package providertunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	connectserver "github.com/urnetwork/server/connect"
	"github.com/urnetwork/server/localclient"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// A local exchange and platform, a real provider client on it, and a probe
// network with its in-process credential and control authority. The provider
// runs as a current provider app does: opportunistic end-to-end sessions with
// a responder, a peer's identity key resolved from the platform's store, and
// receive windows advertised on its acknowledgements. The control API refuses
// every request, and both peers gather WebRTC candidates on loopback only.
type capabilityTestStack struct {
	ProviderClientId server.Id
	SourceClientId   server.Id
	SourceJwt        string
	// the refusing control API
	ApiUrl      string
	PlatformUrl string

	authority *countedProbeAuthority
	apiCalls  *atomic.Int64
}

// The probe network's in-process credentials.
func (self *capabilityTestStack) Credentials() connect.NetworkClientCredentials {
	return self.authority
}

// The probe network's in-process control.
func (self *capabilityTestStack) Control() connect.NetworkClientControl {
	return self.authority
}

// Requests the control API refused.
func (self *capabilityTestStack) ApiCalls() int64 {
	return self.apiCalls.Load()
}

// Derived clients minted, control requests and derived clients retired
// through the probe network's authority.
func (self *capabilityTestStack) AuthorityCalls() (mint int64, control int64, retire int64) {
	return self.authority.mint.Load(), self.authority.control.Load(), self.authority.retire.Load()
}

// Builds the stack under ctx and waits for the provider's provide modes to
// commit; closeStack tears everything down in reverse order.
func newCapabilityTestStack(tb testing.TB, ctx context.Context) (stack *capabilityTestStack, closeStack func()) {
	tb.Helper()
	closes := []func(){}
	closeStack = func() {
		for index := len(closes) - 1; 0 <= index; index -= 1 {
			closes[index]()
		}
	}
	apiCalls := &atomic.Int64{}
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		http.Error(w, "API disabled", 503)
	}))
	closes = append(closes, trap.Close)
	seed := func(label string) (server.Id, string) {
		networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, label, userId)
		model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, label, label)
		code, err := model.CreateBalanceCode(ctx, model.ByteCount(8*1024*1024*1024), 24*time.Hour, 0, label+server.NewId().String(), "", "")
		if err != nil {
			closeStack()
			tb.Fatal(err)
		}
		redeemed, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{Secret: code.Secret, NetworkId: networkId}, ctx)
		if err != nil || redeemed.Error != nil {
			closeStack()
			tb.Fatal("balance redemption", err)
		}
		return clientId, session.NewByJwt(networkId, userId, label, false, false).Client(deviceId, clientId).Testing_Sign()
	}
	providerClientId, providerJwt := seed("capability-provider")
	sourceClientId, sourceJwt := seed("capability-probe")
	providerAuthority, err := localclient.New(ctx, providerJwt, trap.URL)
	if err != nil {
		closeStack()
		tb.Fatal(err)
	}
	closes = append(closes, providerAuthority.Close)
	sourceAuthority, err := localclient.New(ctx, sourceJwt, trap.URL)
	if err != nil {
		closeStack()
		tb.Fatal(err)
	}
	closes = append(closes, sourceAuthority.Close)

	host := "localcapability"
	exchange, err := newPrivateTestExchange(ctx, host)
	if err != nil {
		closeStack()
		tb.Fatal(err)
	}
	handlerSettings := connectserver.DefaultConnectHandlerSettings()
	handlerSettings.ListenH3Port = 0
	handlerSettings.ListenDnsPort = 0
	handlerSettings.ConnectionAnnounceTimeout = 0
	handler := connectserver.NewConnectHandler(ctx, server.NewId(), exchange, handlerSettings)
	platform := httptest.NewServer(router.NewRouter(ctx, []*router.Route{router.NewRoute("GET", "/", handler.Connect)}))
	closes = append(closes, func() { handler.Close(); exchange.Close() }, platform.Close)

	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategy := connect.NewClientStrategy(ctx, strategySettings)
	closes = append(closes, strategy.Close)
	oob := connect.NewApiOutOfBandControlWithLocalControl(ctx, strategy, providerJwt, trap.URL, providerAuthority)
	providerSettings := hermeticWebRtcClientSettings(connect.DefaultClientSettings())
	// a copy, so no settings value another client shares changes
	encryption := *providerSettings.EncryptionSettings
	encryption.Mode = connect.EncryptionModeOpportunistic
	encryption.NewPeerClientPublicKeyFetcher = func(peerId connect.Id) func(context.Context) ([]byte, error) {
		return func(ctx context.Context) ([]byte, error) {
			return model.GetClientPublicKey(ctx, server.Id(peerId))
		}
	}
	providerSettings.EncryptionSettings = &encryption
	provider := connect.NewClient(ctx, connect.Id(providerClientId), oob, providerSettings)
	transport := connect.NewPlatformTransportWithDefaults(provider.Ctx(), strategy, provider.RouteManager(), "ws"+platform.URL[4:], &connect.ClientAuth{ByJwt: providerJwt, InstanceId: connect.NewId(), AppVersion: server.RequireVersion()})
	localNat := connect.NewLocalUserNatWithDefaults(provider.Ctx(), providerClientId.String())
	natSettings := connect.DefaultRemoteUserNatProviderSettings()
	natSettings.SecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
	nat := connect.NewRemoteUserNatProvider(provider, localNat, natSettings)
	closes = append(closes, func() {
		nat.Close()
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer joinCancel()
		_ = transport.CloseAndWait(joinCtx)
		_ = localNat.CloseAndWait(joinCtx)
		_ = provider.CloseAndWait(joinCtx)
		_ = oob.CloseAndWait(joinCtx)
	})
	provider.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{protocol.ProvideMode_Public: true, protocol.ProvideMode_Network: true})
	for registered := false; !registered; {
		modes, err := model.GetProvideModes(ctx, providerClientId)
		registered = err == nil && 0 < len(modes)
		if !registered {
			select {
			case <-ctx.Done():
				closeStack()
				tb.Fatal("provider registration did not commit")
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	return &capabilityTestStack{
		ProviderClientId: providerClientId,
		SourceClientId:   sourceClientId,
		SourceJwt:        sourceJwt,
		ApiUrl:           trap.URL,
		PlatformUrl:      "ws" + platform.URL[4:],
		authority:        &countedProbeAuthority{Authority: sourceAuthority, rejectingApiUrl: trap.URL},
		apiCalls:         apiCalls,
	}, closeStack
}
