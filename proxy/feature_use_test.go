package proxy

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	proxylib "github.com/urnetwork/proxy"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The Pro features notice warns only the networks whose proxies use SOCKS or
// WireGuard (owner, 2026-11-11). Nothing recorded that per proxy, so the proxy
// notes each SOCKS sign-in and each WireGuard handshake (model
// ProxyFeatureUses). These tests pin where it notes them.

type testFeatureUseNote struct {
	proxyId server.Id
	feature string
	useTime time.Time
}

// Records every note the manager makes.
type testFeatureUseNotes struct {
	stateLock sync.Mutex
	notes     []testFeatureUseNote
}

func (self *testFeatureUseNotes) note(proxyId server.Id, feature string, useTime time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.notes = append(self.notes, testFeatureUseNote{proxyId: proxyId, feature: feature, useTime: useTime})
}

func (self *testFeatureUseNotes) taken() []testFeatureUseNote {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	notes := self.notes
	self.notes = nil
	return notes
}

// A sign-in with the proxy's signed credentials notes SOCKS use, whether the
// plan admits it or refuses it: once enforcement is on, a network still
// signing in is told SOCKS stopped. A credential that is not a proxy's signed
// id, or a malformed caller, notes nothing.
func TestSocksSignInNotesSocksUseWhateverThePlanAnswers(t *testing.T) {
	configureHttpBudgetTest(t)
	manager := newHttpConnectTestManager(t)
	notes := &testFeatureUseNotes{}
	manager.featureUseNoter = notes.note
	var planAllows bool
	var planLock sync.Mutex
	manager.proxyFeatureReader = func(ctx context.Context, id server.Id, feature string) bool {
		planLock.Lock()
		defer planLock.Unlock()
		return planAllows
	}
	setPlanAllows := func(allows bool) {
		planLock.Lock()
		defer planLock.Unlock()
		planAllows = allows
	}
	proxyId := server.NewId()
	seedHttpConnectTestLock(manager, proxyId, true)
	core := (&socks5Server{ctx: manager.ctx, settings: DefaultProxySettings(), proxyDeviceManager: manager}).newSocksProxy()

	before := time.Now()
	setPlanAllows(true)
	connect.AssertEqual(t, core.ValidUser(model.SignProxyId(proxyId), "x", "192.0.2.1:1234"), true)
	taken := notes.taken()
	connect.AssertEqual(t, len(taken), 1)
	connect.AssertEqual(t, taken[0].proxyId, proxyId)
	connect.AssertEqual(t, taken[0].feature, model.FeatureSocksProxy)
	connect.AssertEqual(t, !taken[0].useTime.Before(before), true)

	// refused by the plan, as enforcement refuses a network without Pro
	setPlanAllows(false)
	connect.AssertEqual(t, core.ValidUser(model.SignProxyId(proxyId), "x", "192.0.2.1:1234"), false)
	taken = notes.taken()
	connect.AssertEqual(t, len(taken), 1)
	connect.AssertEqual(t, taken[0].proxyId, proxyId)
	connect.AssertEqual(t, taken[0].feature, model.FeatureSocksProxy)

	setPlanAllows(true)
	connect.AssertEqual(t, core.ValidUser("not-a-signed-proxy-id", "x", "192.0.2.1:1234"), false)
	connect.AssertEqual(t, core.ValidUser(model.SignProxyId(proxyId)+"0", "x", "192.0.2.1:1234"), false)
	connect.AssertEqual(t, core.ValidUser(model.SignProxyId(proxyId), "x", "not-an-address"), false)
	connect.AssertEqual(t, len(notes.taken()), 0)

	// a manager without an owner notes nothing and signs in as before
	manager.featureUseNoter = nil
	connect.AssertEqual(t, core.ValidUser(model.SignProxyId(proxyId), "x", "192.0.2.1:1234"), true)
	connect.AssertEqual(t, len(notes.taken()), 0)
}

// The session sampler notes WireGuard use for each proxy whose peer is in
// session, at its newest completed handshake: the address the device reports
// maps to the proxy registered there. A peer that never completed a
// handshake, or whose newest is older than the session window, or an address
// no registered client holds, notes nothing.
func TestWgSessionSampleNotesWireguardUseOfPeersInSession(t *testing.T) {
	ctx := server.WithoutPostgres(context.Background())
	manager := NewProxyDeviceManager(ctx, &ProxyDeviceManagerSettings{NetworkSpace: &sdk.NetworkSpace{}})
	defer func() { _ = manager.CloseAndWait(context.Background()) }()
	notes := &testFeatureUseNotes{}
	manager.featureUseNoter = notes.note
	settings := DefaultProxySettings()
	wg := &wgServer{ctx: ctx, proxyDeviceManager: manager, settings: settings}

	addrA := netip.MustParseAddr("10.30.0.1")
	addrB := netip.MustParseAddr("10.30.0.2")
	addrC := netip.MustParseAddr("10.30.0.3")
	proxyA, proxyB := server.NewId(), server.NewId()
	proxyClients := []*model.ProxyClient{
		{ProxyId: proxyA, WgConfig: &model.WgConfig{ClientIpv4: addrA, ClientPublicKey: "key-a"}},
		// the registered client at B holds another key: not this proxy's peer
		{ProxyId: proxyB, WgConfig: &model.WgConfig{ClientIpv4: addrB, ClientPublicKey: "key-b"}},
		{ProxyId: server.NewId()},
		nil,
	}
	wg.indexWgProxyIds(proxyClients, map[netip.Addr]*proxylib.WgClient{
		addrA: {PublicKey: "key-a", ClientIpv4: addrA},
		addrB: {PublicKey: "key-other", ClientIpv4: addrB},
	})

	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	inSession := now.Add(-time.Minute)
	statuses := map[netip.Addr]*proxylib.WgPeerStatus{
		addrA: {ClientIpv4: addrA, LastHandshake: inSession},
		addrB: {ClientIpv4: addrB, LastHandshake: inSession},
		addrC: {ClientIpv4: addrC, LastHandshake: inSession},
	}
	wg.noteWgFeatureUses(statuses, now)
	connect.AssertEqual(t, notes.taken(), []testFeatureUseNote{
		{proxyId: proxyA, feature: model.FeatureWireguardProxy, useTime: inSession},
	})

	// not in session
	for _, lastHandshake := range []time.Time{{}, now.Add(-settings.WgSessionWindow - time.Second)} {
		wg.noteWgFeatureUses(map[netip.Addr]*proxylib.WgPeerStatus{
			addrA: {ClientIpv4: addrA, LastHandshake: lastHandshake},
		}, now)
	}
	wg.noteWgFeatureUses(map[netip.Addr]*proxylib.WgPeerStatus{addrA: nil}, now)
	connect.AssertEqual(t, len(notes.taken()), 0)

	// the address goes to another proxy: its handshakes are that proxy's
	proxyA2 := server.NewId()
	wg.indexWgProxyIds(
		[]*model.ProxyClient{{ProxyId: proxyA2, WgConfig: &model.WgConfig{ClientIpv4: addrA, ClientPublicKey: "key-a2"}}},
		map[netip.Addr]*proxylib.WgClient{addrA: {PublicKey: "key-a2", ClientIpv4: addrA}},
	)
	wg.noteWgFeatureUses(statuses, now)
	connect.AssertEqual(t, notes.taken(), []testFeatureUseNote{
		{proxyId: proxyA2, feature: model.FeatureWireguardProxy, useTime: inSession},
	})

	// a server without a manager or settings notes nothing
	(&wgServer{ctx: ctx}).noteWgFeatureUses(statuses, now)
	(&wgServer{ctx: ctx, proxyDeviceManager: manager}).noteWgFeatureUses(statuses, now)
	connect.AssertEqual(t, len(notes.taken()), 0)
}

// The real add and sync paths index each valid client's address for the
// sampler; a client that fails validation is not indexed.
func TestWgServerIndexesItsClientsForFeatureUse(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		setProxyTestEnv()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		proxyDeviceManager := NewProxyDeviceManagerWithDefaults(ctx)
		defer func() {
			_ = proxyDeviceManager.CloseAndWait(context.Background())
		}()
		notes := &testFeatureUseNotes{}
		proxyDeviceManager.featureUseNoter = notes.note

		wgCtx, wgCancel := context.WithCancel(ctx)
		defer wgCancel()
		ports, releasePorts := reserveProxyTestPorts(t)
		settings := DefaultProxySettings()
		settings.WgPort = ports.wg
		releasePorts()
		wg := NewWgServer(wgCtx, wgCancel, proxyDeviceManager, settings)

		proxyClientA := newTestProxyClient(t, netip.MustParseAddr("10.40.0.1"))
		proxyClientB := newTestProxyClient(t, netip.MustParseAddr("10.40.0.2"))
		badProxyClient := newTestProxyClient(t, netip.MustParseAddr("10.40.0.3"))
		badProxyClient.AuthToken = model.SignProxyId(server.NewId())
		connect.AssertEqual(t, wg.AddProxyClients(proxyClientA, badProxyClient), nil)
		connect.AssertEqual(t, wg.SyncProxyClients([]*model.ProxyClient{proxyClientA, proxyClientB}, server.NowUtc()), nil)

		now := time.Now()
		statuses := map[netip.Addr]*proxylib.WgPeerStatus{}
		for _, proxyClient := range []*model.ProxyClient{proxyClientA, proxyClientB, badProxyClient} {
			statuses[proxyClient.WgConfig.ClientIpv4] = &proxylib.WgPeerStatus{
				ClientIpv4:    proxyClient.WgConfig.ClientIpv4,
				LastHandshake: now,
			}
		}
		wg.noteWgFeatureUses(statuses, now)
		noted := []server.Id{}
		for _, note := range notes.taken() {
			connect.AssertEqual(t, note.feature, model.FeatureWireguardProxy)
			noted = append(noted, note.proxyId)
		}
		slices.SortFunc(noted, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		expected := []server.Id{proxyClientA.ProxyId, proxyClientB.ProxyId}
		slices.SortFunc(expected, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		connect.AssertEqual(t, noted, expected)
	})
}

// End to end through the proxy's own listeners: an HTTPS request records
// nothing and leaves the free network without the notice; a SOCKS sign-in and
// a WireGuard session are recorded, written by the process owner, and the
// network's proxies list then shows the upgrade notice, as the sdk decides it.
func TestProxyRecordsSocksAndWireguardUseForTheProFeaturesNotice(t *testing.T) {
	if testing.Short() {
		return
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		featureUses := model.NewProxyFeatureUses(context.Background(), model.DefaultProxyFeatureUseSettings())
		defer featureUses.Close()
		opts := defaultProxyTestOptions()
		opts.disableSecurityPolicies = true
		opts.featureUses = featureUses
		h := setupProxyTestWithOptions(t, opts)
		baseUrl, closeTarget := startLocalTarget(t)
		h.targetUrl = baseUrl + "/data?bytes=4096"
		h.targetTlsConfig = testTargetTlsConfig
		h.closeTarget = closeTarget
		defer h.close(t)

		uses := func() map[string]time.Time {
			featureUses.Flush(h.ctx)
			allUses, err := model.GetProxyFeatureUses(h.ctx, []server.Id{h.proxyId})
			connect.AssertEqual(t, err, nil)
			return allUses[h.proxyId]
		}
		listSession := session.Testing_CreateClientSession(h.ctx, &session.ByJwt{NetworkId: h.pdNetworkId})
		prompt := func() string {
			result, err := model.GetNetworkProxies(listSession)
			connect.AssertEqual(t, err, nil)
			data, err := json.Marshal(result)
			connect.AssertEqual(t, err, nil)
			var list sdk.NetworkClientsResult
			connect.AssertEqual(t, json.Unmarshal(data, &list), nil)
			status := sdk.ProFeaturesStatusFor(&list)
			connect.AssertEqual(t, status.PlanKnown, true)
			connect.AssertEqual(t, status.Pro, false)
			return status.Prompt
		}

		testProxyHttps(t, h)
		connect.AssertEqual(t, len(uses()), 0)
		connect.AssertEqual(t, prompt(), sdk.ProFeaturesPromptNone)

		start := time.Now().Add(-time.Second)
		testProxySocks(t, h)
		socksUses := uses()
		connect.AssertEqual(t, len(socksUses), 1)
		connect.AssertEqual(t, !socksUses[model.FeatureSocksProxy].Before(start), true)
		connect.AssertEqual(t, prompt(), sdk.ProFeaturesPromptUpgrade)

		// a WireGuard session, sampled as the session sampler samples it
		wgCtx, wgCancel := context.WithCancel(h.ctx)
		defer wgCancel()
		transport, closeWgClient := startWgClient(t, wgCtx, h.proxyClient.WgConfig, h.wgPort)
		defer closeWgClient()
		requireProxyGet(t, h, "wireguard", transport)
		statuses, err := h.wg.wgProxy.PeerStatuses()
		connect.AssertEqual(t, err, nil)
		h.wg.noteWgFeatureUses(statuses, time.Now())
		bothUses := uses()
		connect.AssertEqual(t, len(bothUses), 2)
		connect.AssertEqual(t, !bothUses[model.FeatureWireguardProxy].Before(start), true)
		connect.AssertEqual(t, prompt(), sdk.ProFeaturesPromptUpgrade)
	})
}
