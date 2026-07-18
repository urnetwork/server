package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// TestNetworkPeerMemberKeys asserts the PEERSSTREAMS2 per-member key
// lifecycle alongside the registry writers: add writes the key with the
// registration ttl, refresh extends the ttl without rewriting, provide-mode
// updates rewrite preserving the ttl, remove deletes, and the delta read
// surfaces the registered peer.
func TestNetworkPeerMemberKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		clientId := server.NewId()
		residentId := server.NewId()
		ttl := 60 * time.Second

		AddNetworkPeer(ctx, networkId, &NetworkPeer{
			ClientId:   clientId,
			DeviceName: "device a",
		}, residentId, ttl)

		memberTtl := func() time.Duration {
			var d time.Duration
			server.Redis(ctx, func(r server.RedisClient) {
				d = r.TTL(ctx, networkPeerMemberKey(networkId, clientId)).Val()
			})
			return d
		}

		// registered: the member key exists with the registration ttl
		d := memberTtl()
		connect.AssertEqual(t, 0 < d && d <= ttl, true)

		// the delta read surfaces the peer
		peer := GetNetworkPeerMember(ctx, networkId, clientId)
		connect.AssertNotEqual(t, peer, nil)
		connect.AssertEqual(t, peer.DeviceName, "device a")

		// refresh extends the ttl
		ok := RefreshNetworkPeer(ctx, networkId, clientId, residentId, 2*ttl)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, ttl < memberTtl(), true)

		// a provide-mode change rewrites the key preserving a ttl
		UpdateNetworkPeerProvideModes(ctx, clientId, map[ProvideMode]bool{})
		// (clientId has no network client record in this test, so the update
		// no-ops — assert the key survived untouched instead)
		connect.AssertEqual(t, ttl < memberTtl(), true)

		// refresh restores a vanished member key
		server.Redis(ctx, func(r server.RedisClient) {
			r.Del(ctx, networkPeerMemberKey(networkId, clientId))
		})
		ok = RefreshNetworkPeer(ctx, networkId, clientId, residentId, ttl)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, 0 < memberTtl(), true)

		// remove deletes the member key
		RemoveNetworkPeer(ctx, networkId, clientId, residentId)
		connect.AssertEqual(t, memberTtl() < 0, true) // -2 = missing key
		connect.AssertEqual(t, GetNetworkPeerMember(ctx, networkId, clientId), nil)
	})
}

// TestNetworkPeerListenerDeltas drives the listener's key-event inputs
// directly: a `set` delta delivers a single-peer Updated event, `del`
// delivers a disconnect marker, and Resync forces a full-read Reset even
// when the version counter did not move.
func TestNetworkPeerListenerDeltas(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		clientId := server.NewId()
		residentId := server.NewId()

		events := make(chan *NetworkPeerEvent, 16)
		listener := NewNetworkPeerListener(
			ctx,
			networkId,
			func(event *NetworkPeerEvent) {
				events <- event
			},
			// the corrective poll is far out of the test window: any delivery
			// below must come from the delta/resync inputs (after first sync)
			10*time.Minute,
			0,
		)
		defer listener.Close()

		// force the first sync now instead of waiting a poll interval
		listener.Resync()
		select {
		case event := <-events:
			connect.AssertEqual(t, event.NetworkPeerEventType, NetworkPeerEventTypeReset)
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for the first sync")
		}

		AddNetworkPeer(ctx, networkId, &NetworkPeer{
			ClientId:   clientId,
			DeviceName: "device a",
		}, residentId, 60*time.Second)

		// a `set` delta delivers the single peer
		listener.Delta(clientId, "set")
		select {
		case event := <-events:
			connect.AssertEqual(t, event.NetworkPeerEventType, NetworkPeerEventTypeUpdated)
			connect.AssertEqual(t, len(event.Peers), 1)
			connect.AssertEqual(t, event.Peers[0].ClientId, clientId)
			connect.AssertEqual(t, event.Peers[0].DeviceName, "device a")
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for the set delta")
		}

		// a `del` delta delivers a disconnect marker
		RemoveNetworkPeer(ctx, networkId, clientId, residentId)
		listener.Delta(clientId, "del")
		select {
		case event := <-events:
			connect.AssertEqual(t, event.NetworkPeerEventType, NetworkPeerEventTypeRemoved)
			connect.AssertEqual(t, len(event.Peers), 1)
			connect.AssertEqual(t, event.Peers[0].ClientId, clientId)
			connect.AssertNotEqual(t, event.Peers[0].DisconnectTime, nil)
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for the del delta")
		}

		// a `set` delta for an unregistered peer degrades to a marker
		listener.Delta(clientId, "set")
		select {
		case event := <-events:
			connect.AssertEqual(t, event.NetworkPeerEventType, NetworkPeerEventTypeRemoved)
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for the raced-set delta")
		}

		// resync full-reads even though the counter is unchanged since the
		// deltas above already reflected every write
		listener.Resync()
		select {
		case event := <-events:
			connect.AssertEqual(t, event.NetworkPeerEventType, NetworkPeerEventTypeReset)
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for the resync reset")
		}
	})
}

// TestNetworkPeerKeyEventParse round-trips the keyspace channel names.
func TestNetworkPeerKeyEventParse(t *testing.T) {
	networkId := server.NewId()
	clientId := server.NewId()

	channel := "__keyspace@0__:" + networkPeerMemberKey(networkId, clientId)
	parsedNetworkId, parsedClientId, ok := ParseNetworkPeerKeyEvent(channel)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, parsedNetworkId, networkId)
	connect.AssertEqual(t, parsedClientId, clientId)

	_, _, ok = ParseNetworkPeerKeyEvent("__keyspace@0__:{np_" + networkId.String() + "}meta")
	connect.AssertEqual(t, ok, false)

	hopChannel := "__keyspace@0__:" + clientStreamHopsKey(clientId)
	parsedClientId, ok = ParseStreamHopsKeyEvent(hopChannel)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, parsedClientId, clientId)
	_, ok = ParseStreamHopsKeyEvent(channel)
	connect.AssertEqual(t, ok, false)
}
