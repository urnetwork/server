package model

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
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
		connect.AssertEqual(t, len(c.Connected()), 0)

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
		connect.AssertEqual(t, eventId, GetNetworkPeerEventId(ctx, networkId))
		connected, markers := splitNetworkPeers(peers)
		connect.AssertEqual(t, len(connected), 2)
		connect.AssertEqual(t, len(markers), 0)
		connect.AssertEqual(t, connected[clientId1].Principal, "svc-a")
		connect.AssertEqual(t, connected[clientId1].Roles, []string{"role1", "role2"})
		connect.AssertEqual(t, connected[clientId1].ProvideModes, []ProvideMode{ProvideModeNetwork, ProvideModeStream})
		connect.AssertEqual(t, connected[clientId1].DeviceName, "device a")
		connect.AssertEqual(t, connected[clientId1].DeviceSpec, "spec a")
		connect.AssertEqual(t, connected[clientId2].Principal, "")
		connect.AssertEqual(t, len(connected[clientId2].Roles), 0)

		// the listener accumulates to the same state
		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, c.Connected(), connected)
		connect.AssertEqual(t, len(c.Markers()), 0)

		// refresh is resident-guarded
		connect.AssertEqual(t, RefreshNetworkPeer(ctx, networkId, clientId1, residentId1, ttl), true)
		connect.AssertEqual(t, RefreshNetworkPeer(ctx, networkId, clientId1, residentId2, ttl), false)
		connect.AssertEqual(t, RefreshNetworkPeer(ctx, networkId, server.NewId(), residentId1, ttl), false)

		// remove is resident-guarded
		RemoveNetworkPeer(ctx, networkId, clientId1, residentId2)
		_, peers = GetNetworkPeers(ctx, networkId)
		connected, _ = splitNetworkPeers(peers)
		connect.AssertEqual(t, len(connected), 2)

		RemoveNetworkPeer(ctx, networkId, clientId1, residentId1)
		_, peers = GetNetworkPeers(ctx, networkId)
		connected, markers = splitNetworkPeers(peers)
		connect.AssertEqual(t, len(connected), 1)
		connect.AssertEqual(t, len(markers), 1)
		connect.AssertNotEqual(t, markers[clientId1].DisconnectTime, nil)

		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, len(c.Connected()), 1)
		connect.AssertEqual(t, len(c.Markers()), 1)

		// a reconnect clears the marker
		AddNetworkPeer(ctx, networkId, peer1, residentId1, ttl)
		_, peers = GetNetworkPeers(ctx, networkId)
		connected, markers = splitNetworkPeers(peers)
		connect.AssertEqual(t, len(connected), 2)
		connect.AssertEqual(t, len(markers), 0)

		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, c.Connected(), connected)
		connect.AssertEqual(t, len(c.Markers()), 0)

		// v2 (PEERS2.md): delivery is poll + full-read — every emitted event
		// is a Reset snapshot (the accumulator diffs locally); incremental
		// Updated/Removed events are no longer delivered
		connect.AssertNotEqual(t, len(c.events), 0)
		for _, event := range c.events {
			connect.AssertEqual(t, event.NetworkPeerEventType, NetworkPeerEventTypeReset)
		}

		// a new listener syncs to the head state with a reset
		c2 := newTestNetworkPeerAccumulator()
		listener2 := NewNetworkPeerListener(ctx, networkId, c2.Event, 200*time.Millisecond, 5)
		defer listener2.Close()

		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, c2.Connected(), connected)
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
		connect.AssertEqual(t, len(connected), 0)
		connect.AssertEqual(t, len(markers), 1)
		connect.AssertNotEqual(t, markers[clientId1].DisconnectTime, nil)

		// another peer's activity prunes the expired entry and publishes
		// the disconnect marker
		c := newTestNetworkPeerAccumulator()
		listener := NewNetworkPeerListener(ctx, networkId, c.Event, 200*time.Millisecond, 5)
		defer listener.Close()

		AddNetworkPeer(ctx, networkId, peer2, residentId2, 60*time.Second)

		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, len(c.Connected()), 1)
		connect.AssertEqual(t, len(c.Markers()), 1)

		// the pruned registration is gone, so refresh reports not registered
		connect.AssertEqual(t, RefreshNetworkPeer(ctx, networkId, clientId1, residentId1, 60*time.Second), false)
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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, authClientResult.Error, nil)
		clientId = *authClientResult.ClientId

		_, topLevel, _, profile, _ := GetNetworkPeerProfile(ctx, clientId)
		connect.AssertEqual(t, topLevel, true)
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
		connect.AssertEqual(t, len(connected), 1)
		connect.AssertEqual(t, connected[clientId].ProvideModes, []ProvideMode{ProvideModeNetwork, ProvideModeStream})

		// no change publishes no event
		eventCount := c.EventCount()
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeNetwork: []byte("network-key"),
			ProvideModeStream:  []byte("stream-key"),
		})
		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, c.EventCount(), eventCount)

		// removing provide keys publishes the reduced modes
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeStream: []byte("stream-key"),
		})
		select {
		case <-time.After(1 * time.Second):
		}
		connected = c.Connected()
		connect.AssertEqual(t, connected[clientId].ProvideModes, []ProvideMode{ProvideModeStream})
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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, authClientResult.Error, nil)
		clientId := *authClientResult.ClientId

		profileNetworkId, topLevel, category, profile, peersEnabled := GetNetworkPeerProfile(ctx, clientId)
		connect.AssertEqual(t, profileNetworkId, networkId)
		connect.AssertEqual(t, topLevel, true)
		// a network under the top-level limit is enabled for peers
		connect.AssertEqual(t, peersEnabled, true)
		// an ordinary client is the client category
		connect.AssertEqual(t, category, NetworkPeerCategoryClient)
		connect.AssertEqual(t, profile.ClientId, clientId)
		// roles are deduped and sorted
		connect.AssertEqual(t, profile.Roles, []string{"role1", "role2"})
		connect.AssertEqual(t, profile.Principal, "svc-a")
		connect.AssertEqual(t, profile.DeviceName, "test device")
		connect.AssertEqual(t, profile.DeviceSpec, "test spec")

		// the identity read-through matches
		identity := GetClientIdentity(ctx, clientId)
		connect.AssertEqual(t, identity.Roles, []string{"role1", "role2"})
		connect.AssertEqual(t, identity.Principal, "svc-a")
		// and again from the cache
		identity = GetClientIdentity(ctx, clientId)
		connect.AssertEqual(t, identity.Roles, []string{"role1", "role2"})
		connect.AssertEqual(t, identity.Principal, "svc-a")

		// a derivative client is not top-level
		sourceClientResult, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description:    "derived device",
				SourceClientId: &clientId,
			},
			userSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, sourceClientResult.Error, nil)

		_, topLevel, _, profile, peersEnabled = GetNetworkPeerProfile(ctx, *sourceClientResult.ClientId)
		connect.AssertEqual(t, topLevel, false)
		connect.AssertNotEqual(t, profile, nil)
		// a derivative client never resolves peers enabled
		connect.AssertEqual(t, peersEnabled, false)

		// a guest session cannot assign roles or principal
		guestSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
			GuestMode: true,
		})
		authClientResult, err = AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "guest device",
				Roles:       []string{"role1"},
			},
			guestSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, authClientResult.Error, nil)

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
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, authClientResult.Error, nil)

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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, authClientResult.Error, nil)

		_, _, _, profile, _ = GetNetworkPeerProfile(ctx, *authClientResult.ClientId)
		connect.AssertEqual(t, profile.Roles, []string{"service-role"})
		connect.AssertEqual(t, profile.Principal, "svc-inherited")
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
		connect.AssertEqual(t, err, nil)
		clientId := *clientResult.ClientId

		// a proxy client: a top-level client with a proxy_device_config row
		proxyResult, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "proxy"}, userSession)
		connect.AssertEqual(t, err, nil)
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
		connect.AssertEqual(t, topLevel, true)
		connect.AssertEqual(t, category, NetworkPeerCategoryProxy)
		connect.AssertNotEqual(t, profile, nil)
		connect.AssertEqual(t, peersEnabled, true)
		_, _, clientCategory, _, _ := GetNetworkPeerProfile(ctx, clientId)
		connect.AssertEqual(t, clientCategory, NetworkPeerCategoryClient)

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
		connect.AssertEqual(t, len(connected), 1)
		connect.AssertNotEqual(t, connected[clientId], nil)
		connect.AssertEqual(t, connected[proxyClientId], nil)

		// the listener only saw the client peer
		listenerConnected := c.Connected()
		connect.AssertEqual(t, len(listenerConnected), 1)
		connect.AssertNotEqual(t, listenerConnected[clientId], nil)
		connect.AssertEqual(t, listenerConnected[proxyClientId], nil)

		// the combined count includes both
		connect.AssertEqual(t, GetNetworkConnectedCount(ctx, networkId), 2)

		// removing the proxy peer drops the count but emits no marker/event
		eventCount := c.EventCount()
		RemoveNetworkProxyPeer(ctx, networkId, proxyClientId)
		connect.AssertEqual(t, GetNetworkConnectedCount(ctx, networkId), 1)
		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, c.EventCount(), eventCount)
		connect.AssertEqual(t, len(c.Markers()), 0)
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
		connect.AssertEqual(t, len(c.Connected()), 1)

		// create a delivery gap: advance the event counter without publishing
		server.Redis(ctx, func(r server.RedisClient) {
			for range 2 {
				_, err := r.Incr(ctx, networkPeerEventIdKey(networkId)).Result()
				connect.AssertEqual(t, err, nil)
			}
		})

		// the next published event id is not contiguous, so the listener
		// resets and converges to the full state
		AddNetworkPeer(ctx, networkId, &NetworkPeer{ClientId: clientId2}, residentId, ttl)

		select {
		case <-time.After(2 * time.Second):
		}
		connected := c.Connected()
		connect.AssertEqual(t, len(connected), 2)
		connect.AssertNotEqual(t, connected[clientId1], nil)
		connect.AssertNotEqual(t, connected[clientId2], nil)

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
		connect.AssertEqual(t, 2 <= resetCount, true)
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
		connect.AssertEqual(t, len(c.Connected()), 1)

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
			connect.AssertEqual(t, err, nil)
		})

		// the registration is lost: the heartbeat refresh reports not
		// registered, and the caller re-adds (the resident recovery branch).
		// Recover to a DIFFERENT peer with exactly one add: the restarted
		// counter lands on the same value the listener already synced (1), so
		// the version comparison cannot see the change — only the forced
		// insurance full-read delivers it. (Recovering to the same peer would
		// let stale accumulator state mask a suppressed delivery.)
		connect.AssertEqual(t, RefreshNetworkPeer(ctx, networkId, clientId, residentId, ttl), false)
		clientId2 := server.NewId()
		residentId2 := server.NewId()
		AddNetworkPeer(ctx, networkId, &NetworkPeer{ClientId: clientId2, Principal: "svc-b"}, residentId2, ttl)

		// the registry recovered, with the counter back at an already-synced value
		recoveredEventId, peers := GetNetworkPeers(ctx, networkId)
		connect.AssertEqual(t, recoveredEventId, int64(1))
		connected, _ := splitNetworkPeers(peers)
		connect.AssertEqual(t, len(connected), 1)
		connect.AssertEqual(t, connected[clientId2].Principal, "svc-b")

		// the already-subscribed listener re-delivers via the insurance
		// full-read despite the matching version, rather than staying stale
		select {
		case <-time.After(3 * time.Second):
		}
		connectedAccumulated := c.Connected()
		connect.AssertEqual(t, len(connectedAccumulated), 1)
		connect.AssertNotEqual(t, connectedAccumulated[clientId2], nil)

		// a fresh listener converges too
		c2 := newTestNetworkPeerAccumulator()
		listener2 := NewNetworkPeerListener(ctx, networkId, c2.Event, 200*time.Millisecond, 5)
		defer listener2.Close()

		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, len(c2.Connected()), 1)
	})
}

// TestNetworkPeerListenerNoConnectionGrowth guards the PEERS2 property whose
// absence caused the 2026-07-15 outage: the poll listeners must NOT open a
// standing connection per listener (v1 held one pubsub subscription each,
// O(clients) connections that melted the cluster). Many listeners share the
// pool; connected_clients stays ~pool-sized, and idle polls (no registry
// change) deliver no events.
func TestNetworkPeerListenerNoConnectionGrowth(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clientsInfo := func(field string) int {
			count := -1
			server.Redis(ctx, func(r server.RedisClient) {
				info, err := r.Info(ctx, "clients").Result()
				connect.AssertEqual(t, err, nil)
				for _, line := range strings.Split(info, "\n") {
					if strings.HasPrefix(line, field+":") {
						fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, field+":")), "%d", &count)
					}
				}
			})
			return count
		}
		connectedClients := func() int { return clientsInfo("connected_clients") }

		baseline := connectedClients()

		// stand up many listeners on distinct networks, each with one peer
		const listenerCount = 40
		accumulators := make([]*testNetworkPeerAccumulator, listenerCount)
		for i := range listenerCount {
			networkId := server.NewId()
			AddNetworkPeer(ctx, networkId, &NetworkPeer{ClientId: server.NewId()}, server.NewId(), 60*time.Second)
			c := newTestNetworkPeerAccumulator()
			accumulators[i] = c
			listener := NewNetworkPeerListener(ctx, networkId, c.Event, 200*time.Millisecond, 5)
			defer listener.Close()
		}

		// let every listener poll many times (2s / 200ms = ~10 ticks each)
		select {
		case <-time.After(2 * time.Second):
		}

		// connections did NOT grow ~1 per listener. The pool cap (test config
		// max_connections=16) plus a small margin is the ceiling regardless of
		// listener count — v1 would have added ~40 subscription connections.
		grown := connectedClients() - baseline
		if grown >= listenerCount/2 {
			t.Fatalf("connected_clients grew by %d for %d listeners — listeners are not sharing the pool (v1 regression?)", grown, listenerCount)
		}

		// the defining v2 invariant, literally: ZERO pubsub subscriptions
		// exist while listeners run (v1 held one per listener)
		if n := clientsInfo("pubsub_clients"); n != 0 {
			t.Fatalf("pubsub_clients = %d, want 0 — a subscription-based listener path is back (v1 regression)", n)
		}

		// every listener synced its one peer
		for i, c := range accumulators {
			if len(c.Connected()) != 1 {
				t.Fatalf("listener %d saw %d peers, want 1", i, len(c.Connected()))
			}
			// idle polls (no registry change after the initial sync) do not
			// deliver per tick: only the initial reset + the 1/5 insurance
			// full-reads (~2-3 over 2s), never one event per poll tick (~10)
			if n := c.EventCount(); n > 6 {
				t.Fatalf("listener %d delivered %d events for a static registry — polls are over-delivering", i, n)
			}
		}
	})
}

// TestNetworkPeerListenerSurvivesRedisError guards the dead-listener fix: a
// redis error inside a poll must be contained to that tick (logged, backed
// off), never kill the listener. 2026-07-15: a panic in the listener run
// goroutine killed it permanently and the client silently stopped receiving
// peer updates.
func TestNetworkPeerListenerSurvivesRedisError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		clientId1 := server.NewId()

		c := newTestNetworkPeerAccumulator()
		listener := NewNetworkPeerListener(ctx, networkId, c.Event, 200*time.Millisecond, 5)
		defer listener.Close()

		AddNetworkPeer(ctx, networkId, &NetworkPeer{ClientId: clientId1}, server.NewId(), 60*time.Second)
		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, len(c.Connected()), 1)

		// corrupt the version counter to a non-integer: every poll's
		// GetNetworkPeerEventId now panics on the Int64 parse
		server.Redis(ctx, func(r server.RedisClient) {
			connect.AssertEqual(t, r.Set(ctx, networkPeerEventIdKey(networkId), "not-a-number", 0).Err(), nil)
		})

		// the listener rides several failing polls without dying
		select {
		case <-time.After(2 * time.Second):
		}

		// recover: drop the corrupt counter (a legitimate reset). The next
		// poll reads a missing (0) counter, mismatches its synced value, and
		// resyncs from the intact registry — proving the listener survived the
		// error window and still delivers.
		clientId2 := server.NewId()
		server.Redis(ctx, func(r server.RedisClient) {
			connect.AssertEqual(t, r.Del(ctx, networkPeerEventIdKey(networkId)).Err(), nil)
		})
		AddNetworkPeer(ctx, networkId, &NetworkPeer{ClientId: clientId2}, server.NewId(), 60*time.Second)

		select {
		case <-time.After(3 * time.Second):
		}
		connect.AssertEqual(t, len(c.Connected()), 2)
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
		connect.AssertEqual(t, len(registryConnected), peerCount/2)
		connect.AssertEqual(t, len(registryMarkers), peerCount-peerCount/2)
		for clientId, endConnected := range expectedConnected {
			if endConnected {
				connect.AssertNotEqual(t, registryConnected[clientId], nil)
			} else {
				connect.AssertNotEqual(t, registryMarkers[clientId], nil)
			}
		}

		// the accumulated listener state converges to the registry truth
		connect.AssertEqual(t, c.Connected(), registryConnected)
		for clientId := range registryMarkers {
			connect.AssertNotEqual(t, c.Markers()[clientId], nil)
		}

		// a fresh listener converges to the same state
		c2 := newTestNetworkPeerAccumulator()
		listener2 := NewNetworkPeerListener(ctx, networkId, c2.Event, 200*time.Millisecond, 5)
		defer listener2.Close()

		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, c2.Connected(), registryConnected)
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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, authClientResult.Error, nil)
		clientId := *authClientResult.ClientId

		// re-auth mints the client's stored identity into the client jwt
		reauthResult, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				ClientId:    &clientId,
				Description: "renamed device",
			},
			userSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, reauthResult.Error, nil)
		reauthByJwt, err := jwt.ParseByJwt(ctx, *reauthResult.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, reauthByJwt.Roles, []string{"role1", "role2"})
		connect.AssertEqual(t, reauthByJwt.Principal, "svc-a")

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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, reauthResult.Error, nil)
		reauthByJwt, err = jwt.ParseByJwt(ctx, *reauthResult.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, reauthByJwt.Roles, []string{"role1", "role2"})
		connect.AssertEqual(t, reauthByJwt.Principal, "svc-a")

		// roles and principal are immutable post-create
		reauthResult, err = AuthNetworkClient(
			&AuthNetworkClientArgs{
				ClientId:  &clientId,
				Roles:     []string{"role3"},
				Principal: "svc-b",
			},
			userSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, reauthResult.Error, nil)
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

		// the top-level client cap is gated by enforce_concurrent_clients (dark by
		// default; see pro.yml). Enable it so this test exercises the cap. No
		// client is connected here, so the plan concurrent-connected limit stays
		// at zero and never interferes.
		defer Testing_SetEnforceConcurrentClients(true)()
		Testing_ClearNetworkPeersEnabledCache()

		var firstClientId server.Id
		for i := range LimitTopLevelClientIdsPerNetwork {
			authClientResult, err := AuthNetworkClient(
				&AuthNetworkClientArgs{
					Description: "test device",
				},
				userSession,
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, authClientResult.Error, nil)
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
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, authClientResult.Error, nil)
		connect.AssertEqual(t, authClientResult.Error.ClientLimitExceeded, true)

		// derivative clients are not limited by the top-level limit
		authClientResult, err = AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description:    "derived device",
				SourceClientId: &firstClientId,
			},
			userSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, authClientResult.Error, nil)

		// a network at the limit still gets peer subscriptions
		connect.AssertEqual(t, NetworkPeersEnabled(ctx, networkId), true)

		// a network over the limit (created before the limit) does not.
		// The decision is cached per network, so the cached value holds until
		// the ttl (cleared here)
		Testing_CreateDevice(ctx, networkId, server.NewId(), server.NewId(), "grandfathered", "grandfathered")
		connect.AssertEqual(t, NetworkPeersEnabled(ctx, networkId), true)
		Testing_ClearNetworkPeersEnabledCache()
		connect.AssertEqual(t, NetworkPeersEnabled(ctx, networkId), false)

		// the profile resolves the same decision
		_, topLevel, _, profile, peersEnabled := GetNetworkPeerProfile(ctx, firstClientId)
		connect.AssertEqual(t, topLevel, true)
		connect.AssertNotEqual(t, profile, nil)
		connect.AssertEqual(t, peersEnabled, false)
	})
}

// TestNetworkTopLevelClientLimitDisabled is the counterpart to the test above:
// while the concurrent-client limit is DISABLED (dark by default in prod), a
// network must be able to connect MORE than LimitTopLevelClientIdsPerNetwork
// top-level clients — a network with a large provider fleet must never have a
// provider refused. Every admission gate is dark in this state:
// AuthNetworkClient (creation), NetworkConcurrentClientsExceeded (plan), and
// CanConnectNetworkPeer (connection activation).
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
			connect.AssertEqual(t, err, nil)
			if authClientResult.Error != nil {
				t.Fatalf("provider %d refused while limit disabled: %s (ClientLimitExceeded=%v)",
					i, authClientResult.Error.Message, authClientResult.Error.ClientLimitExceeded)
			}
			clientIds = append(clientIds, *authClientResult.ClientId)
		}
		connect.AssertEqual(t, len(clientIds), overLimit)

		// the plan gates report no limit while disabled, at a count over the cap
		connect.AssertEqual(t, NetworkConcurrentClientsExceeded(ctx, networkId), false)
		// and an over-limit network still gets peer registrations (poll
		// architecture handles any size; PEERS2.md)
		connect.AssertEqual(t, NetworkPeersEnabled(ctx, networkId), true)
		// every client may connect — the activation gate is dark
		for i, clientId := range clientIds {
			if !CanConnectNetworkPeer(ctx, clientId) {
				t.Fatalf("provider %d (%s) cannot connect while limit disabled", i, clientId)
			}
		}
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
		connect.AssertEqual(t, GetNetworkEnforceableConnectedCount(ctx, networkId), 1)

		// a second ordinary client would exceed the limit -> refused
		ordinary2Id := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), ordinary2Id, "ordinary2", "ordinary2")
		connect.AssertEqual(t, CanConnectNetworkPeer(ctx, ordinary2Id), false)

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
		connect.AssertEqual(t, GetNetworkEnforceableConnectedCount(ctx, networkId), 1)
	})
}
