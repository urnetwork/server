package model

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// collects listener events and accumulates them into the peer state a client
// would hold, so tests can compare the accumulated state to the model state
type testNetworkPeerAccumulator struct {
	stateLock sync.Mutex
	events    []*NetworkPeerEvent
	connected map[server.Id]*NetworkPeer
	markers   map[server.Id]*NetworkPeer
}

func newTestNetworkPeerAccumulator() *testNetworkPeerAccumulator {
	return &testNetworkPeerAccumulator{
		connected: map[server.Id]*NetworkPeer{},
		markers:   map[server.Id]*NetworkPeer{},
	}
}

func (self *testNetworkPeerAccumulator) Event(event *NetworkPeerEvent) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.events = append(self.events, event)
	if event.NetworkPeerEventType == NetworkPeerEventTypeReset {
		clear(self.connected)
		clear(self.markers)
	}
	for _, peer := range event.Peers {
		if peer.DisconnectTime != nil {
			delete(self.connected, peer.ClientId)
			self.markers[peer.ClientId] = peer
		} else {
			delete(self.markers, peer.ClientId)
			self.connected[peer.ClientId] = peer
		}
	}
}

func (self *testNetworkPeerAccumulator) Connected() map[server.Id]*NetworkPeer {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	connected := map[server.Id]*NetworkPeer{}
	for clientId, peer := range self.connected {
		connected[clientId] = peer
	}
	return connected
}

func (self *testNetworkPeerAccumulator) Markers() map[server.Id]*NetworkPeer {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	markers := map[server.Id]*NetworkPeer{}
	for clientId, peer := range self.markers {
		markers[clientId] = peer
	}
	return markers
}

func (self *testNetworkPeerAccumulator) EventCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.events)
}

func splitNetworkPeers(peers []*NetworkPeer) (connected map[server.Id]*NetworkPeer, markers map[server.Id]*NetworkPeer) {
	connected = map[server.Id]*NetworkPeer{}
	markers = map[server.Id]*NetworkPeer{}
	for _, peer := range peers {
		if peer.DisconnectTime != nil {
			markers[peer.ClientId] = peer
		} else {
			connected[peer.ClientId] = peer
		}
	}
	return
}

func TestNetworkPeerLifecycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		clientId1 := server.NewId()
		clientId2 := server.NewId()
		residentId1 := server.NewId()
		residentId2 := server.NewId()
		ttl := 60 * time.Second

		c := newTestNetworkPeerAccumulator()
		listener := NewNetworkPeerListener(ctx, networkId, c.Event, 200*time.Millisecond, 5)
		defer listener.Close()

		// the listener syncs an empty reset on subscribe
		select {
		case <-time.After(1 * time.Second):
		}
		assert.Equal(t, len(c.Connected()), 0)

		peer1 := &NetworkPeer{
			ClientId:     clientId1,
			ProvideModes: []ProvideMode{ProvideModeNetwork, ProvideModeStream},
			Principal:    "svc-a",
			Roles:        []string{"role1", "role2"},
			DeviceName:   "device a",
			DeviceSpec:   "spec a",
		}
		peer2 := &NetworkPeer{
			ClientId:     clientId2,
			ProvideModes: []ProvideMode{ProvideModeStream},
		}

		AddNetworkPeer(ctx, networkId, peer1, residentId1, ttl)
		AddNetworkPeer(ctx, networkId, peer2, residentId2, ttl)

		eventId, peers := GetNetworkPeers(ctx, networkId)
		assert.Equal(t, eventId, GetNetworkPeerEventId(ctx, networkId))
		connected, markers := splitNetworkPeers(peers)
		assert.Equal(t, len(connected), 2)
		assert.Equal(t, len(markers), 0)
		assert.Equal(t, connected[clientId1].Principal, "svc-a")
		assert.Equal(t, connected[clientId1].Roles, []string{"role1", "role2"})
		assert.Equal(t, connected[clientId1].ProvideModes, []ProvideMode{ProvideModeNetwork, ProvideModeStream})
		assert.Equal(t, connected[clientId1].DeviceName, "device a")
		assert.Equal(t, connected[clientId1].DeviceSpec, "spec a")
		assert.Equal(t, connected[clientId2].Principal, "")
		assert.Equal(t, len(connected[clientId2].Roles), 0)

		// the listener accumulates to the same state
		select {
		case <-time.After(1 * time.Second):
		}
		assert.Equal(t, c.Connected(), connected)
		assert.Equal(t, len(c.Markers()), 0)

		// refresh is resident-guarded
		assert.Equal(t, RefreshNetworkPeer(ctx, networkId, clientId1, residentId1, ttl), true)
		assert.Equal(t, RefreshNetworkPeer(ctx, networkId, clientId1, residentId2, ttl), false)
		assert.Equal(t, RefreshNetworkPeer(ctx, networkId, server.NewId(), residentId1, ttl), false)

		// remove is resident-guarded
		RemoveNetworkPeer(ctx, networkId, clientId1, residentId2)
		_, peers = GetNetworkPeers(ctx, networkId)
		connected, _ = splitNetworkPeers(peers)
		assert.Equal(t, len(connected), 2)

		RemoveNetworkPeer(ctx, networkId, clientId1, residentId1)
		_, peers = GetNetworkPeers(ctx, networkId)
		connected, markers = splitNetworkPeers(peers)
		assert.Equal(t, len(connected), 1)
		assert.Equal(t, len(markers), 1)
		assert.NotEqual(t, markers[clientId1].DisconnectTime, nil)

		select {
		case <-time.After(1 * time.Second):
		}
		assert.Equal(t, len(c.Connected()), 1)
		assert.Equal(t, len(c.Markers()), 1)

		// a reconnect clears the marker
		AddNetworkPeer(ctx, networkId, peer1, residentId1, ttl)
		_, peers = GetNetworkPeers(ctx, networkId)
		connected, markers = splitNetworkPeers(peers)
		assert.Equal(t, len(connected), 2)
		assert.Equal(t, len(markers), 0)

		select {
		case <-time.After(1 * time.Second):
		}
		assert.Equal(t, c.Connected(), connected)
		assert.Equal(t, len(c.Markers()), 0)

		// event ids are monotonic with no gaps, so the listener never resets
		// after the initial subscribe
		eventTypes := []NetworkPeerEventType{}
		for _, event := range c.events {
			eventTypes = append(eventTypes, event.NetworkPeerEventType)
		}
		assert.Equal(t, eventTypes[0], NetworkPeerEventTypeReset)
		assert.Equal(t, slices.Contains(eventTypes[1:], NetworkPeerEventTypeReset), false)

		// a new listener syncs to the head state with a reset
		c2 := newTestNetworkPeerAccumulator()
		listener2 := NewNetworkPeerListener(ctx, networkId, c2.Event, 200*time.Millisecond, 5)
		defer listener2.Close()

		select {
		case <-time.After(1 * time.Second):
		}
		assert.Equal(t, c2.Connected(), connected)
	})
}

func TestNetworkPeerExpiry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		clientId1 := server.NewId()
		clientId2 := server.NewId()
		residentId1 := server.NewId()
		residentId2 := server.NewId()

		peer1 := &NetworkPeer{
			ClientId: clientId1,
		}
		peer2 := &NetworkPeer{
			ClientId: clientId2,
		}

		// register with a short ttl and let it expire
		AddNetworkPeer(ctx, networkId, peer1, residentId1, 500*time.Millisecond)

		select {
		case <-time.After(1 * time.Second):
		}

		// expired but not yet pruned entries read as disconnect markers
		_, peers := GetNetworkPeers(ctx, networkId)
		connected, markers := splitNetworkPeers(peers)
		assert.Equal(t, len(connected), 0)
		assert.Equal(t, len(markers), 1)
		assert.NotEqual(t, markers[clientId1].DisconnectTime, nil)

		// another peer's activity prunes the expired entry and publishes
		// the disconnect marker
		c := newTestNetworkPeerAccumulator()
		listener := NewNetworkPeerListener(ctx, networkId, c.Event, 200*time.Millisecond, 5)
		defer listener.Close()

		AddNetworkPeer(ctx, networkId, peer2, residentId2, 60*time.Second)

		select {
		case <-time.After(1 * time.Second):
		}
		assert.Equal(t, len(c.Connected()), 1)
		assert.Equal(t, len(c.Markers()), 1)

		// the pruned registration is gone, so refresh reports not registered
		assert.Equal(t, RefreshNetworkPeer(ctx, networkId, clientId1, residentId1, 60*time.Second), false)
	})
}

func TestNetworkPeerProvideModesUpdate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		clientId := server.NewId()
		residentId := server.NewId()
		userId := server.NewId()

		Testing_CreateNetwork(ctx, networkId, "test", userId)
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})
		authClientResult, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "test device",
				DeviceSpec:  "test spec",
			},
			userSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, authClientResult.Error, nil)
		clientId = *authClientResult.ClientId

		_, topLevel, _, profile, _ := GetNetworkPeerProfile(ctx, clientId)
		assert.Equal(t, topLevel, true)
		AddNetworkPeer(ctx, networkId, profile, residentId, 60*time.Second)

		c := newTestNetworkPeerAccumulator()
		listener := NewNetworkPeerListener(ctx, networkId, c.Event, 200*time.Millisecond, 5)
		defer listener.Close()

		// SetProvide publishes a provide modes update for the registered peer
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeNetwork: []byte("network-key"),
			ProvideModeStream:  []byte("stream-key"),
		})

		select {
		case <-time.After(1 * time.Second):
		}
		connected := c.Connected()
		assert.Equal(t, len(connected), 1)
		assert.Equal(t, connected[clientId].ProvideModes, []ProvideMode{ProvideModeNetwork, ProvideModeStream})

		// no change publishes no event
		eventCount := c.EventCount()
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeNetwork: []byte("network-key"),
			ProvideModeStream:  []byte("stream-key"),
		})
		select {
		case <-time.After(1 * time.Second):
		}
		assert.Equal(t, c.EventCount(), eventCount)

		// removing provide keys publishes the reduced modes
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeStream: []byte("stream-key"),
		})
		select {
		case <-time.After(1 * time.Second):
		}
		connected = c.Connected()
		assert.Equal(t, connected[clientId].ProvideModes, []ProvideMode{ProvideModeStream})
	})
}

func TestNetworkPeerProfile(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		userId := server.NewId()

		Testing_CreateNetwork(ctx, networkId, "test", userId)
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})

		// a top-level client with roles and principal
		authClientResult, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "test device",
				DeviceSpec:  "test spec",
				Roles:       []string{"role2", "role1", "role1"},
				Principal:   "svc-a",
			},
			userSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, authClientResult.Error, nil)
		clientId := *authClientResult.ClientId

		profileNetworkId, topLevel, category, profile, peersEnabled := GetNetworkPeerProfile(ctx, clientId)
		assert.Equal(t, profileNetworkId, networkId)
		assert.Equal(t, topLevel, true)
		// a network under the top-level limit is enabled for peers
		assert.Equal(t, peersEnabled, true)
		// an ordinary client is the client category
		assert.Equal(t, category, NetworkPeerCategoryClient)
		assert.Equal(t, profile.ClientId, clientId)
		// roles are deduped and sorted
		assert.Equal(t, profile.Roles, []string{"role1", "role2"})
		assert.Equal(t, profile.Principal, "svc-a")
		assert.Equal(t, profile.DeviceName, "test device")
		assert.Equal(t, profile.DeviceSpec, "test spec")

		// the identity read-through matches
		identity := GetClientIdentity(ctx, clientId)
		assert.Equal(t, identity.Roles, []string{"role1", "role2"})
		assert.Equal(t, identity.Principal, "svc-a")
		// and again from the cache
		identity = GetClientIdentity(ctx, clientId)
		assert.Equal(t, identity.Roles, []string{"role1", "role2"})
		assert.Equal(t, identity.Principal, "svc-a")

		// a derivative client is not top-level
		sourceClientResult, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description:    "derived device",
				SourceClientId: &clientId,
			},
			userSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, sourceClientResult.Error, nil)

		_, topLevel, _, profile, peersEnabled = GetNetworkPeerProfile(ctx, *sourceClientResult.ClientId)
		assert.Equal(t, topLevel, false)
		assert.NotEqual(t, profile, nil)
		// a derivative client never resolves peers enabled
		assert.Equal(t, peersEnabled, false)

		// a guest session cannot assign roles or principal
		guestSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
			GuestMode: false,
		})
		authClientResult, err = AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "guest device",
				Roles:       []string{"role1"},
			},
			guestSession,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, authClientResult.Error, nil)

		// a client session cannot assign roles or principal
		deviceId := server.NewId()
		clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
			DeviceId:  &deviceId,
			ClientId:  &clientId,
		})
		authClientResult, err = AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "client device",
				Principal:   "svc-b",
			},
			clientSession,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, authClientResult.Error, nil)

		// a session with roles and principal (e.g. from an auth code) passes
		// them to clients it creates
		serviceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
			Roles:     []string{"service-role"},
			Principal: "svc-inherited",
		})
		authClientResult, err = AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "service device",
			},
			serviceSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, authClientResult.Error, nil)

		_, _, _, profile, _ = GetNetworkPeerProfile(ctx, *authClientResult.ClientId)
		assert.Equal(t, profile.Roles, []string{"service-role"})
		assert.Equal(t, profile.Principal, "svc-inherited")
	})
}

func TestNetworkProxyPeer(t *testing.T) {
	// proxy clients count toward a network's connected total but never appear
	// in the peer list and emit no peer events
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("test-%s", networkId), userId)
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})

		// an ordinary client
		clientResult, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "client"}, userSession)
		assert.Equal(t, err, nil)
		clientId := *clientResult.ClientId

		// a proxy client: a top-level client with a proxy_device_config row
		proxyResult, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "proxy"}, userSession)
		assert.Equal(t, err, nil)
		proxyClientId := *proxyResult.ClientId
		proxyInstanceId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO proxy_device_config (proxy_id, client_id, instance_id, config_json)
					VALUES ($1, $2, $3, '{}')
				`,
				server.NewId(),
				proxyClientId,
				proxyInstanceId,
			))
		})

		// the profile detects the proxy category
		_, topLevel, category, profile, peersEnabled := GetNetworkPeerProfile(ctx, proxyClientId)
		assert.Equal(t, topLevel, true)
		assert.Equal(t, category, NetworkPeerCategoryProxy)
		assert.NotEqual(t, profile, nil)
		assert.Equal(t, peersEnabled, true)
		_, _, clientCategory, _, _ := GetNetworkPeerProfile(ctx, clientId)
		assert.Equal(t, clientCategory, NetworkPeerCategoryClient)

		residentId := server.NewId()
		ttl := 60 * time.Second

		// a listener sees the client peer but never the proxy peer
		c := newTestNetworkPeerAccumulator()
		listener := NewNetworkPeerListener(ctx, networkId, c.Event, 200*time.Millisecond, 5)
		defer listener.Close()

		AddNetworkPeer(ctx, networkId, &NetworkPeer{ClientId: clientId}, residentId, ttl)
		AddNetworkProxyPeer(ctx, networkId, proxyClientId, ttl)

		select {
		case <-time.After(1 * time.Second):
		}

		// the peer list contains only the client
		_, peers := GetNetworkPeers(ctx, networkId)
		connected, _ := splitNetworkPeers(peers)
		assert.Equal(t, len(connected), 1)
		assert.NotEqual(t, connected[clientId], nil)
		assert.Equal(t, connected[proxyClientId], nil)

		// the listener only saw the client peer
		listenerConnected := c.Connected()
		assert.Equal(t, len(listenerConnected), 1)
		assert.NotEqual(t, listenerConnected[clientId], nil)
		assert.Equal(t, listenerConnected[proxyClientId], nil)

		// the combined count includes both
		assert.Equal(t, GetNetworkConnectedCount(ctx, networkId), 2)

		// removing the proxy peer drops the count but emits no marker/event
		eventCount := c.EventCount()
		RemoveNetworkProxyPeer(ctx, networkId, proxyClientId)
		assert.Equal(t, GetNetworkConnectedCount(ctx, networkId), 1)
		select {
		case <-time.After(1 * time.Second):
		}
		assert.Equal(t, c.EventCount(), eventCount)
		assert.Equal(t, len(c.Markers()), 0)
	})
}

func TestNetworkPeerEventGapReset(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		clientId1 := server.NewId()
		clientId2 := server.NewId()
		residentId := server.NewId()
		ttl := 60 * time.Second

		c := newTestNetworkPeerAccumulator()
		listener := NewNetworkPeerListener(ctx, networkId, c.Event, 200*time.Millisecond, 5)
		defer listener.Close()

		AddNetworkPeer(ctx, networkId, &NetworkPeer{ClientId: clientId1}, residentId, ttl)

		select {
		case <-time.After(1 * time.Second):
		}
		assert.Equal(t, len(c.Connected()), 1)

		// create a delivery gap: advance the event counter without publishing
		server.Redis(ctx, func(r server.RedisClient) {
			for range 2 {
				_, err := r.Incr(ctx, networkPeerEventIdKey(networkId)).Result()
				assert.Equal(t, err, nil)
			}
		})

		// the next published event id is not contiguous, so the listener
		// resets and converges to the full state
		AddNetworkPeer(ctx, networkId, &NetworkPeer{ClientId: clientId2}, residentId, ttl)

		select {
		case <-time.After(2 * time.Second):
		}
		connected := c.Connected()
		assert.Equal(t, len(connected), 2)
		assert.NotEqual(t, connected[clientId1], nil)
		assert.NotEqual(t, connected[clientId2], nil)

		// the reset event was used to recover
		resetCount := 0
		func() {
			c.stateLock.Lock()
			defer c.stateLock.Unlock()
			for _, event := range c.events {
				if event.NetworkPeerEventType == NetworkPeerEventTypeReset {
					resetCount += 1
				}
			}
		}()
		// initial subscribe reset plus the gap recovery reset
		assert.Equal(t, 2 <= resetCount, true)
	})
}

func TestNetworkPeerRegistryFlushRecovery(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		clientId := server.NewId()
		residentId := server.NewId()
		ttl := 60 * time.Second

		peer := &NetworkPeer{
			ClientId:  clientId,
			Principal: "svc-a",
		}

		c := newTestNetworkPeerAccumulator()
		listener := NewNetworkPeerListener(ctx, networkId, c.Event, 200*time.Millisecond, 5)
		defer listener.Close()

		AddNetworkPeer(ctx, networkId, peer, residentId, ttl)

		select {
		case <-time.After(1 * time.Second):
		}
		assert.Equal(t, len(c.Connected()), 1)

		// flush the registry (e.g. a redis loss). The event counter restarts,
		// so subsequent event ids move backward.
		server.Redis(ctx, func(r server.RedisClient) {
			err := r.Del(
				ctx,
				networkPeerMetaKey(networkId),
				networkPeerConnectedKey(networkId),
				networkPeerDisconnectedKey(networkId),
				networkPeerEventIdKey(networkId),
			).Err()
			assert.Equal(t, err, nil)
		})

		// the registration is lost: the heartbeat refresh reports not
		// registered. A flush and rebuild are not atomic in production (the
		// heartbeat re-adds over seconds), so the listener first polls the
		// intermediate empty state (missing counter reads as 0, below the
		// synced value -> resync to empty), then the re-add diverges the
		// version again.
		assert.Equal(t, RefreshNetworkPeer(ctx, networkId, clientId, residentId, ttl), false)
		select {
		case <-time.After(1 * time.Second):
		}
		assert.Equal(t, len(c.Connected()), 0)

		// the resident recovery branch re-adds (a different peer here to show
		// the resync carries fresh data, not stale accumulator state)
		clientId2 := server.NewId()
		residentId2 := server.NewId()
		AddNetworkPeer(ctx, networkId, &NetworkPeer{ClientId: clientId2, Principal: "svc-b"}, residentId2, ttl)

		_, peers := GetNetworkPeers(ctx, networkId)
		connected, _ := splitNetworkPeers(peers)
		assert.Equal(t, len(connected), 1)
		assert.Equal(t, connected[clientId2].Principal, "svc-b")

		// the already-subscribed listener resyncs to the rebuilt state
		select {
		case <-time.After(3 * time.Second):
		}
		connectedAccumulated := c.Connected()
		assert.Equal(t, len(connectedAccumulated), 1)
		assert.NotEqual(t, connectedAccumulated[clientId2], nil)

		// a fresh listener converges too
		c2 := newTestNetworkPeerAccumulator()
		listener2 := NewNetworkPeerListener(ctx, networkId, c2.Event, 200*time.Millisecond, 5)
		defer listener2.Close()

		select {
		case <-time.After(1 * time.Second):
		}
		assert.Equal(t, len(c2.Connected()), 1)
	})
}

func TestNetworkPeerChurn(t *testing.T) {
	// concurrently churn add/refresh/remove/provide-update across many peers
	// of one network and assert the listener-accumulated state converges to
	// the registry truth
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("test-%s", networkId), userId)

		peerCount := 32
		opCount := 24
		ttl := 120 * time.Second

		// real client rows so the provide-update network lookup resolves
		clientIds := []server.Id{}
		for range peerCount {
			clientId := server.NewId()
			deviceId := server.NewId()
			Testing_CreateDevice(ctx, networkId, deviceId, clientId, "churn", "churn")
			clientIds = append(clientIds, clientId)
		}

		c := newTestNetworkPeerAccumulator()
		listener := NewNetworkPeerListener(ctx, networkId, c.Event, 200*time.Millisecond, 5)
		defer listener.Close()

		// each peer churns on its own goroutine; half end connected,
		// half end removed
		expectedConnected := map[server.Id]bool{}
		var wg sync.WaitGroup
		for i, clientId := range clientIds {
			endConnected := i%2 == 0
			expectedConnected[clientId] = endConnected
			residentId := server.NewId()
			wg.Add(1)
			go func() {
				defer wg.Done()

				peer := &NetworkPeer{
					ClientId:  clientId,
					Principal: fmt.Sprintf("svc-%s", clientId),
				}
				AddNetworkPeer(ctx, networkId, peer, residentId, ttl)
				for op := range opCount {
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Duration(mathrand.Intn(20)) * time.Millisecond):
					}
					switch op % 4 {
					case 0:
						RefreshNetworkPeer(ctx, networkId, clientId, residentId, ttl)
					case 1:
						UpdateNetworkPeerProvideModes(ctx, clientId, map[ProvideMode]bool{
							ProvideModeNetwork: op%8 == 1,
							ProvideModeStream:  true,
						})
					case 2:
						RemoveNetworkPeer(ctx, networkId, clientId, residentId)
					case 3:
						AddNetworkPeer(ctx, networkId, peer, residentId, ttl)
					}
				}
				if endConnected {
					AddNetworkPeer(ctx, networkId, peer, residentId, ttl)
				} else {
					RemoveNetworkPeer(ctx, networkId, clientId, residentId)
				}
			}()
		}
		wg.Wait()

		// let the event stream drain
		select {
		case <-time.After(3 * time.Second):
		}

		_, peers := GetNetworkPeers(ctx, networkId)
		registryConnected, registryMarkers := splitNetworkPeers(peers)

		// the registry truth matches the intended end state
		assert.Equal(t, len(registryConnected), peerCount/2)
		assert.Equal(t, len(registryMarkers), peerCount-peerCount/2)
		for clientId, endConnected := range expectedConnected {
			if endConnected {
				assert.NotEqual(t, registryConnected[clientId], nil)
			} else {
				assert.NotEqual(t, registryMarkers[clientId], nil)
			}
		}

		// the accumulated listener state converges to the registry truth
		assert.Equal(t, c.Connected(), registryConnected)
		for clientId := range registryMarkers {
			assert.NotEqual(t, c.Markers()[clientId], nil)
		}

		// a fresh listener converges to the same state
		c2 := newTestNetworkPeerAccumulator()
		listener2 := NewNetworkPeerListener(ctx, networkId, c2.Event, 200*time.Millisecond, 5)
		defer listener2.Close()

		select {
		case <-time.After(1 * time.Second):
		}
		assert.Equal(t, c2.Connected(), registryConnected)
	})
}

func TestNetworkClientReauthIdentity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		userId := server.NewId()

		Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("test-%s", networkId), userId)
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})

		authClientResult, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "test device",
				Roles:       []string{"role1", "role2"},
				Principal:   "svc-a",
			},
			userSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, authClientResult.Error, nil)
		clientId := *authClientResult.ClientId

		// re-auth mints the client's stored identity into the client jwt
		reauthResult, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				ClientId:    &clientId,
				Description: "renamed device",
			},
			userSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, reauthResult.Error, nil)
		reauthByJwt, err := jwt.ParseByJwt(ctx, *reauthResult.ByClientJwt)
		assert.Equal(t, err, nil)
		assert.Equal(t, reauthByJwt.Roles, []string{"role1", "role2"})
		assert.Equal(t, reauthByJwt.Principal, "svc-a")

		// a session with its own identity claims does not override the
		// client's stored identity on re-auth
		serviceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
			Roles:     []string{"other-role"},
			Principal: "svc-other",
		})
		reauthResult, err = AuthNetworkClient(
			&AuthNetworkClientArgs{
				ClientId:    &clientId,
				Description: "renamed again",
			},
			serviceSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, reauthResult.Error, nil)
		reauthByJwt, err = jwt.ParseByJwt(ctx, *reauthResult.ByClientJwt)
		assert.Equal(t, err, nil)
		assert.Equal(t, reauthByJwt.Roles, []string{"role1", "role2"})
		assert.Equal(t, reauthByJwt.Principal, "svc-a")

		// roles and principal are immutable post-create
		reauthResult, err = AuthNetworkClient(
			&AuthNetworkClientArgs{
				ClientId:  &clientId,
				Roles:     []string{"role3"},
				Principal: "svc-b",
			},
			userSession,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, reauthResult.Error, nil)
	})
}

func TestNetworkPeerTopLevelClientLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		userId := server.NewId()

		Testing_CreateNetwork(ctx, networkId, "test", userId)
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})

		var firstClientId server.Id
		for i := range LimitTopLevelClientIdsPerNetwork {
			authClientResult, err := AuthNetworkClient(
				&AuthNetworkClientArgs{
					Description: "test device",
				},
				userSession,
			)
			assert.Equal(t, err, nil)
			assert.Equal(t, authClientResult.Error, nil)
			if i == 0 {
				firstClientId = *authClientResult.ClientId
			}
		}

		// the next top-level create exceeds the limit
		authClientResult, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "one too many",
			},
			userSession,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, authClientResult.Error, nil)
		assert.Equal(t, authClientResult.Error.ClientLimitExceeded, true)

		// derivative clients are not limited by the top-level limit
		authClientResult, err = AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description:    "derived device",
				SourceClientId: &firstClientId,
			},
			userSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, authClientResult.Error, nil)

		// a network at the limit still gets peer subscriptions
		assert.Equal(t, NetworkPeersEnabled(ctx, networkId), true)

		// a network over the limit (created before the limit) does not.
		// The decision is cached per network, so the cached value holds until
		// the ttl (cleared here)
		Testing_CreateDevice(ctx, networkId, server.NewId(), server.NewId(), "grandfathered", "grandfathered")
		assert.Equal(t, NetworkPeersEnabled(ctx, networkId), true)
		Testing_ClearNetworkPeersEnabledCache()
		assert.Equal(t, NetworkPeersEnabled(ctx, networkId), false)

		// the profile resolves the same decision
		_, topLevel, _, profile, peersEnabled := GetNetworkPeerProfile(ctx, firstClientId)
		assert.Equal(t, topLevel, true)
		assert.NotEqual(t, profile, nil)
		assert.Equal(t, peersEnabled, false)
	})
}

// TestNetworkTopLevelClientLimitDisabled is the counterpart to the test above:
// while the concurrent-client limit is DISABLED (dark by default in prod), a
// network must be able to connect MORE than LimitTopLevelClientIdsPerNetwork
// top-level clients — a network with a large provider fleet must never have a
// provider refused. The connection/creation gates are all dark in this state:
// AuthNetworkClient (creation), NetworkConcurrentClientsExceeded (plan), and
// CanConnectNetworkPeer (connection activation).
//
// The peer-feature valve (NetworkPeersEnabled), however, is enforced
// INDEPENDENTLY of enforce_concurrent_clients (the 2026-07-17 fix): an
// over-limit network connects normally but gets peersEnabled=false, so its
// O(size^2) peer full-read fan-out never lands on a single redis shard.
func TestNetworkTopLevelClientLimitDisabled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "test", userId)
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})

		// explicitly disabled (also the prod default) + a tiny plan limit that
		// MUST NOT bite while disabled
		defer Testing_SetEnforceConcurrentClients(false)()
		defer Testing_SetConcurrentClientsLimit(1, 1)()
		Testing_ClearNetworkPeersEnabledCache()

		// create well beyond the top-level limit — every provider connects
		const overLimit = LimitTopLevelClientIdsPerNetwork + 25
		clientIds := make([]server.Id, 0, overLimit)
		for i := range overLimit {
			authClientResult, err := AuthNetworkClient(
				&AuthNetworkClientArgs{Description: fmt.Sprintf("provider %d", i)},
				userSession,
			)
			assert.Equal(t, err, nil)
			if authClientResult.Error != nil {
				t.Fatalf("provider %d refused while limit disabled: %s (ClientLimitExceeded=%v)",
					i, authClientResult.Error.Message, authClientResult.Error.ClientLimitExceeded)
			}
			clientIds = append(clientIds, *authClientResult.ClientId)
		}
		assert.Equal(t, len(clientIds), overLimit)

		// the plan gates report no limit while disabled, at a count over the cap
		assert.Equal(t, NetworkConcurrentClientsExceeded(ctx, networkId), false)
		// every client may connect — the connection activation gate is dark
		for i, clientId := range clientIds {
			if !CanConnectNetworkPeer(ctx, clientId) {
				t.Fatalf("provider %d (%s) cannot connect while limit disabled", i, clientId)
			}
		}
		// BUT the peer valve is enforced independently of
		// enforce_concurrent_clients: an over-limit network gets NO peer
		// registrations/subscriptions even while disabled, so its O(size^2)
		// full-read fan-out never lands on redis (2026-07-17 fix).
		assert.Equal(t, NetworkPeersEnabled(ctx, networkId), false)
		// the per-client profile resolves the same peers-off decision while
		// still reporting the client as a valid top-level client
		_, topLevel, _, profile, peersEnabled := GetNetworkPeerProfile(ctx, clientIds[0])
		assert.Equal(t, topLevel, true)
		assert.NotEqual(t, profile, nil)
		assert.Equal(t, peersEnabled, false)
	})
}

// TestNetworkProviderConnectionExemptFromLimit guards the specific provider
// concern under FUTURE enforcement: even when the concurrent-client limit is
// ENABLED and the network is at its plan limit, PUBLIC PROVIDERS can still
// connect — they add capacity rather than consume it, so they are exempt from
// both the enforceable connected count and the activation gate. A network's
// providers are never blocked by its own client limit.
func TestNetworkProviderConnectionExemptFromLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()

		// enforcement ON, plan limit of 1 connected top-level client
		defer Testing_SetEnforceConcurrentClients(true)()
		defer Testing_SetConcurrentClientsLimit(1, 1)()
		Testing_ClearNetworkPeersEnabledCache()

		// register one ordinary (non-provider) connected client: the network is
		// now at its plan limit of 1. (Testing_CreateDevice arg order is
		// networkId, deviceId, clientId.)
		ordinaryId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), ordinaryId, "ordinary", "ordinary")
		AddNetworkPeer(ctx, networkId, &NetworkPeer{ClientId: ordinaryId}, server.NewId(), 60*time.Second)
		assert.Equal(t, GetNetworkEnforceableConnectedCount(ctx, networkId), 1)

		// a second ordinary client would exceed the limit -> refused
		ordinary2Id := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), ordinary2Id, "ordinary2", "ordinary2")
		assert.Equal(t, CanConnectNetworkPeer(ctx, ordinary2Id), false)

		// a PUBLIC PROVIDER connects regardless: exempt from the count and the
		// activation gate. SetProvide gives the DB provide modes the gate reads;
		// register several beyond the limit.
		publicStream := map[ProvideMode][]byte{
			ProvideModePublic: make([]byte, 32),
			ProvideModeStream: make([]byte, 32),
		}
		for i := range 5 {
			providerId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), providerId,
				fmt.Sprintf("provider %d", i), fmt.Sprintf("provider %d", i))
			SetProvide(ctx, providerId, publicStream)
			AddNetworkPeer(ctx, networkId, &NetworkPeer{
				ClientId:     providerId,
				ProvideModes: []ProvideMode{ProvideModePublic, ProvideModeStream},
			}, server.NewId(), 60*time.Second)
			if !CanConnectNetworkPeer(ctx, providerId) {
				t.Fatalf("public provider %d refused at the network's client limit", i)
			}
		}

		// providers did not consume the enforceable count (still 1: the lone
		// ordinary client)
		assert.Equal(t, GetNetworkEnforceableConnectedCount(ctx, networkId), 1)
	})
}
