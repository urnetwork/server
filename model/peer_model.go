package model

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// the network peer registry stores the set of connected top-level clients per
// network (clients with no `source_client_id`) and identity metadata for each:
// enabled provide modes, principal, and roles. The resident registers its
// client on nomination, heartbeats it on the resident poll, and removes it on
// close (residentId-guarded, like the resident registry). Listeners subscribe
// per network and mirror the `StreamHopListener` pattern: reset on subscribe,
// diff events with a monotonic event id, reset on an event id gap, and a poll
// fallback.

// how long a disconnected peer is reported after disconnect
const NetworkPeerDisconnectedWindow = 5 * time.Minute

// safety ttl for the per-network keys, refreshed on peer activity,
// so networks with no connected peers eventually clear
const networkPeerKeyTtl = 24 * time.Hour

// a peer of a network: a connected top-level client and its identity metadata.
// when `DisconnectTime` is set, the entry is a disconnect marker for a
// recently disconnected peer.
type NetworkPeer struct {
	ClientId     server.Id     `json:"client_id"`
	ProvideModes []ProvideMode `json:"provide_modes,omitempty"`
	Principal    string        `json:"principal,omitempty"`
	Roles        []string      `json:"roles,omitempty"`
	DeviceName   string        `json:"device_name,omitempty"`
	DeviceSpec   string        `json:"device_spec,omitempty"`

	DisconnectTime *time.Time `json:"disconnect_time,omitempty"`
}

// NetworkPeerCategory distinguishes ordinary clients from hosted proxy clients
// in the peer registry. Both count toward a network's connected client total,
// but only clients appear in the peer list and receive peer subscriptions: a
// hosted proxy device is controlled remotely and does not participate as a
// visible peer.
type NetworkPeerCategory int

const (
	NetworkPeerCategoryClient NetworkPeerCategory = 0
	NetworkPeerCategoryProxy  NetworkPeerCategory = 1
)

// use gob encoding for `networkPeerMeta` which is more compact than json
type networkPeerMeta struct {
	Peer *NetworkPeer
	// the resident that registered the peer, used to guard remove and refresh
	ResidentId server.Id
}

// note all keys for a network share the {np_<networkId>} hash tag
// so they can be used in the same pipeline and eval (clustered redis)

// hash: client id bytes -> gob `networkPeerMeta`
func networkPeerMetaKey(networkId server.Id) string {
	return fmt.Sprintf("{np_%s}meta", networkId)
}

// zset: client id bytes scored by expiry unix milli
func networkPeerConnectedKey(networkId server.Id) string {
	return fmt.Sprintf("{np_%s}connected", networkId)
}

// zset: client id bytes scored by disconnect unix milli
func networkPeerDisconnectedKey(networkId server.Id) string {
	return fmt.Sprintf("{np_%s}disconnected", networkId)
}

// zset: proxy client id bytes scored by expiry unix milli. Proxy clients count
// toward a network's connected total but never appear in the peer list and get
// no events/markers/subscription, so they live in a separate zset that the
// client peer flow never reads.
func networkPeerConnectedProxyKey(networkId server.Id) string {
	return fmt.Sprintf("{np_%s}connected_proxy", networkId)
}

func networkPeerEventIdKey(networkId server.Id) string {
	return fmt.Sprintf("{np_%s}eid", networkId)
}

func networkPeerEventsKey(networkId server.Id) string {
	return fmt.Sprintf("{np_%s}events", networkId)
}

func loadNetworkPeerMeta(metaBytes []byte) (*networkPeerMeta, error) {
	if len(metaBytes) == 0 {
		return nil, nil
	}
	var meta networkPeerMeta
	err := gob.NewDecoder(bytes.NewBuffer(metaBytes)).Decode(&meta)
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

func (self *networkPeerMeta) Bytes() []byte {
	buf := bytes.NewBuffer(nil)
	err := gob.NewEncoder(buf).Encode(self)
	if err != nil {
		panic(err)
	}
	return buf.Bytes()
}

type NetworkPeerEventType int

const (
	NetworkPeerEventTypeUpdated NetworkPeerEventType = 1
	NetworkPeerEventTypeRemoved NetworkPeerEventType = 2
	NetworkPeerEventTypeReset   NetworkPeerEventType = 3
)

type NetworkPeerEvent struct {
	EventId int64
	// updated, removed, reset
	NetworkPeerEventType NetworkPeerEventType
	// for removed, the entries are disconnect markers
	Peers []*NetworkPeer
}

func publishNetworkPeerEvent(
	ctx context.Context,
	r server.RedisClient,
	networkId server.Id,
	eventType NetworkPeerEventType,
	peers []*NetworkPeer,
) {
	if len(peers) == 0 {
		return
	}

	eventId, err := r.Incr(ctx, networkPeerEventIdKey(networkId)).Result()
	if err != nil {
		panic(err)
	}

	event := &NetworkPeerEvent{
		EventId:              eventId,
		NetworkPeerEventType: eventType,
		Peers:                peers,
	}
	buf := bytes.NewBuffer(nil)
	err = gob.NewEncoder(buf).Encode(event)
	if err != nil {
		panic(err)
	}

	pipe := r.TxPipeline()
	pipe.SPublish(ctx, networkPeerEventsKey(networkId), buf.Bytes())
	pipe.Expire(ctx, networkPeerEventIdKey(networkId), networkPeerKeyTtl)
	_, err = pipe.Exec(ctx)
	if err != nil {
		panic(err)
	}
}

// NetworkPeersEnabled returns whether the network is within the top-level
// client limit. Networks above the limit (created before the limit) do not
// get peer registrations or subscriptions, since the peer replay and event
// fan-out scale with the number of connected top-level clients.
func NetworkPeersEnabled(ctx context.Context, networkId server.Id) (enabled bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT COUNT(*) AS top_level_client_count
				FROM network_client
				WHERE
					network_id = $1 AND
					active = true AND
					source_client_id IS NULL
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				topLevelClientCount := 0
				server.Raise(result.Scan(&topLevelClientCount))
				enabled = topLevelClientCount <= LimitTopLevelClientIdsPerNetwork
			}
		})
	})
	return
}

// GetNetworkPeerProfile loads the network, top-level status, category, and
// identity metadata used to register a client in the peer registry.
// `peer` is nil when the client does not exist or is not active. `category` is
// proxy when the client has a hosted proxy device (a proxy_device_config row).
func GetNetworkPeerProfile(ctx context.Context, clientId server.Id) (networkId server.Id, topLevel bool, category NetworkPeerCategory, peer *NetworkPeer) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_client.network_id,
					network_client.source_client_id,
					network_client.principal,
					device.device_name,
					device.device_spec,
					EXISTS (
						SELECT 1 FROM proxy_device_config
						WHERE proxy_device_config.client_id = network_client.client_id
					) AS is_proxy
				FROM network_client
				LEFT JOIN device ON
					device.device_id = network_client.device_id
				WHERE
					network_client.client_id = $1 AND
					network_client.active = true
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var sourceClientId *server.Id
				var principal *string
				var deviceName *string
				var deviceSpec *string
				var isProxy bool
				server.Raise(result.Scan(
					&networkId,
					&sourceClientId,
					&principal,
					&deviceName,
					&deviceSpec,
					&isProxy,
				))
				topLevel = sourceClientId == nil
				if isProxy {
					category = NetworkPeerCategoryProxy
				}
				peer = &NetworkPeer{
					ClientId: clientId,
				}
				if principal != nil {
					peer.Principal = *principal
				}
				if deviceName != nil {
					peer.DeviceName = *deviceName
				}
				if deviceSpec != nil {
					peer.DeviceSpec = *deviceSpec
				}
			}
		})

		if peer == nil {
			return
		}

		result, err = conn.Query(
			ctx,
			`
				SELECT role FROM network_client_role
				WHERE client_id = $1
				ORDER BY role
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var role string
				server.Raise(result.Scan(&role))
				peer.Roles = append(peer.Roles, role)
			}
		})
	})

	if peer != nil {
		provideModes, err := GetProvideModes(ctx, clientId)
		if err == nil {
			peer.ProvideModes = sortedProvideModesList(provideModes)
		}
	}

	return
}

func sortedProvideModesList(provideModes map[ProvideMode]bool) []ProvideMode {
	provideModesList := []ProvideMode{}
	for provideMode, allow := range provideModes {
		if allow {
			provideModesList = append(provideModesList, provideMode)
		}
	}
	slices.Sort(provideModesList)
	return provideModesList
}

// AddNetworkPeer registers a connected top-level client in the peer registry
// and publishes an updated event
func AddNetworkPeer(
	ctx context.Context,
	networkId server.Id,
	peer *NetworkPeer,
	residentId server.Id,
	ttl time.Duration,
) {
	meta := &networkPeerMeta{
		Peer:       peer,
		ResidentId: residentId,
	}
	member := string(peer.ClientId.Bytes())
	expiryMs := server.NowUtc().Add(ttl).UnixMilli()

	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		pipe.HSet(ctx, networkPeerMetaKey(networkId), member, meta.Bytes())
		pipe.ZAdd(ctx, networkPeerConnectedKey(networkId), redis.Z{
			Score:  float64(expiryMs),
			Member: member,
		})
		pipe.ZRem(ctx, networkPeerDisconnectedKey(networkId), member)
		pipe.Expire(ctx, networkPeerMetaKey(networkId), networkPeerKeyTtl)
		pipe.Expire(ctx, networkPeerConnectedKey(networkId), networkPeerKeyTtl)
		pipe.Expire(ctx, networkPeerDisconnectedKey(networkId), networkPeerKeyTtl)
		_, err := pipe.Exec(ctx)
		if err != nil {
			panic(err)
		}

		publishNetworkPeerEvent(ctx, r, networkId, NetworkPeerEventTypeUpdated, []*NetworkPeer{peer})

		pruneNetworkPeers(ctx, r, networkId)
	})
}

// RefreshNetworkPeer extends the connected expiry of a registered peer.
// Returns false when the peer is not registered by `residentId`,
// in which case the caller should re-add the peer.
func RefreshNetworkPeer(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	residentId server.Id,
	ttl time.Duration,
) (ok bool) {
	member := string(clientId.Bytes())

	server.Redis(ctx, func(r server.RedisClient) {
		metaBytes, _ := r.HGet(ctx, networkPeerMetaKey(networkId), member).Bytes()
		meta, _ := loadNetworkPeerMeta(metaBytes)
		if meta == nil || meta.ResidentId != residentId {
			return
		}

		expiryMs := server.NowUtc().Add(ttl).UnixMilli()
		pipe := r.TxPipeline()
		pipe.ZAdd(ctx, networkPeerConnectedKey(networkId), redis.Z{
			Score:  float64(expiryMs),
			Member: member,
		})
		pipe.Expire(ctx, networkPeerMetaKey(networkId), networkPeerKeyTtl)
		pipe.Expire(ctx, networkPeerConnectedKey(networkId), networkPeerKeyTtl)
		pipe.Expire(ctx, networkPeerDisconnectedKey(networkId), networkPeerKeyTtl)
		_, err := pipe.Exec(ctx)
		if err != nil {
			panic(err)
		}
		ok = true

		pruneNetworkPeers(ctx, r, networkId)
	})
	return
}

// RemoveNetworkPeer removes a peer from the registry and publishes a
// disconnect marker. The remove applies only when the peer is still
// registered by `residentId`, so a replaced resident cannot remove the
// replacement's registration.
func RemoveNetworkPeer(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	residentId server.Id,
) {
	member := string(clientId.Bytes())

	server.Redis(ctx, func(r server.RedisClient) {
		metaBytes, _ := r.HGet(ctx, networkPeerMetaKey(networkId), member).Bytes()
		meta, _ := loadNetworkPeerMeta(metaBytes)
		if meta == nil || meta.ResidentId != residentId {
			return
		}

		disconnectTime := server.NowUtc()
		pipe := r.TxPipeline()
		pipe.HDel(ctx, networkPeerMetaKey(networkId), member)
		pipe.ZRem(ctx, networkPeerConnectedKey(networkId), member)
		pipe.ZAdd(ctx, networkPeerDisconnectedKey(networkId), redis.Z{
			Score:  float64(disconnectTime.UnixMilli()),
			Member: member,
		})
		pipe.Expire(ctx, networkPeerDisconnectedKey(networkId), networkPeerKeyTtl)
		_, err := pipe.Exec(ctx)
		if err != nil {
			panic(err)
		}

		publishNetworkPeerEvent(ctx, r, networkId, NetworkPeerEventTypeRemoved, []*NetworkPeer{
			networkPeerDisconnectMarker(clientId, disconnectTime),
		})
	})
}

// AddNetworkProxyPeer registers a connected hosted proxy client. Proxy clients
// count toward the network's connected total but never appear in the peer list
// and emit no events; they live in a separate zset (see
// networkPeerConnectedProxyKey). Refresh by calling again with a fresh ttl.
func AddNetworkProxyPeer(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	ttl time.Duration,
) {
	member := string(clientId.Bytes())
	expiryMs := server.NowUtc().Add(ttl).UnixMilli()

	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		pipe.ZAdd(ctx, networkPeerConnectedProxyKey(networkId), redis.Z{
			Score:  float64(expiryMs),
			Member: member,
		})
		pipe.Expire(ctx, networkPeerConnectedProxyKey(networkId), networkPeerKeyTtl)
		_, err := pipe.Exec(ctx)
		if err != nil {
			panic(err)
		}
		pruneNetworkProxyPeers(ctx, r, networkId)
	})
}

// RemoveNetworkProxyPeer removes a connected hosted proxy client.
func RemoveNetworkProxyPeer(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
) {
	member := string(clientId.Bytes())
	server.Redis(ctx, func(r server.RedisClient) {
		err := r.ZRem(ctx, networkPeerConnectedProxyKey(networkId), member).Err()
		if err != nil {
			panic(err)
		}
	})
}

// pruneNetworkProxyPeers ages out expired proxy entries. Piggybacks on proxy
// peer activity, like pruneNetworkPeers for clients.
func pruneNetworkProxyPeers(ctx context.Context, r server.RedisClient, networkId server.Id) {
	nowMs := server.NowUtc().UnixMilli()
	err := r.ZRemRangeByScore(
		ctx,
		networkPeerConnectedProxyKey(networkId),
		"-inf",
		strconv.FormatInt(nowMs, 10),
	).Err()
	if err != nil {
		panic(err)
	}
}

// GetNetworkConnectedCount returns the number of connected top-level clients of
// a network, counting both ordinary clients and hosted proxy clients. This is
// the combined connected total a client+proxy quota would enforce against; it
// is exposed for accounting and not enforced here.
func GetNetworkConnectedCount(ctx context.Context, networkId server.Id) (count int) {
	server.Redis(ctx, func(r server.RedisClient) {
		nowMs := server.NowUtc().UnixMilli()
		// count only entries whose expiry is still in the future
		liveMin := strconv.FormatInt(nowMs+1, 10)

		pipe := r.TxPipeline()
		clientCmd := pipe.ZCount(ctx, networkPeerConnectedKey(networkId), liveMin, "+inf")
		proxyCmd := pipe.ZCount(ctx, networkPeerConnectedProxyKey(networkId), liveMin, "+inf")
		_, err := pipe.Exec(ctx)
		if err != nil && err != server.RedisNil {
			panic(err)
		}
		count = int(clientCmd.Val()) + int(proxyCmd.Val())
	})
	return
}

// UpdateNetworkPeerProvideModes updates the provide modes of a registered
// peer and publishes an updated event. No-op when the client is not a
// registered peer or the modes did not change.
func UpdateNetworkPeerProvideModes(
	ctx context.Context,
	clientId server.Id,
	provideModes map[ProvideMode]bool,
) {
	networkId := GetNetworkClientNetwork(ctx, clientId)
	if networkId == nil {
		return
	}
	member := string(clientId.Bytes())
	provideModesList := sortedProvideModesList(provideModes)

	server.Redis(ctx, func(r server.RedisClient) {
		metaBytes, _ := r.HGet(ctx, networkPeerMetaKey(*networkId), member).Bytes()
		meta, _ := loadNetworkPeerMeta(metaBytes)
		if meta == nil {
			// not a registered peer
			return
		}
		if slices.Equal(meta.Peer.ProvideModes, provideModesList) {
			// no change
			return
		}

		meta.Peer.ProvideModes = provideModesList
		err := r.HSet(ctx, networkPeerMetaKey(*networkId), member, meta.Bytes()).Err()
		if err != nil {
			panic(err)
		}

		publishNetworkPeerEvent(ctx, r, *networkId, NetworkPeerEventTypeUpdated, []*NetworkPeer{meta.Peer})
	})
}

func networkPeerDisconnectMarker(clientId server.Id, disconnectTime time.Time) *NetworkPeer {
	disconnectTime = disconnectTime.UTC()
	return &NetworkPeer{
		ClientId:       clientId,
		DisconnectTime: &disconnectTime,
	}
}

// pruneNetworkPeers moves expired connected peers to disconnect markers and
// ages out markers older than the disconnected window. Piggybacks on peer
// activity (add/refresh), so a crashed resident's peer is pruned by the other
// residents of the network. A concurrent refresh can race the prune at the
// expiry boundary; the refresh then re-adds on its next poll (self-healing).
func pruneNetworkPeers(ctx context.Context, r server.RedisClient, networkId server.Id) {
	nowMs := server.NowUtc().UnixMilli()

	expired, err := r.ZRangeByScoreWithScores(ctx, networkPeerConnectedKey(networkId), &redis.ZRangeBy{
		Min: "-inf",
		Max: strconv.FormatInt(nowMs, 10),
	}).Result()
	if err != nil {
		panic(err)
	}

	if 0 < len(expired) {
		markers := []*NetworkPeer{}
		pipe := r.TxPipeline()
		for _, z := range expired {
			member := z.Member.(string)
			clientId := server.Id([]byte(member))
			expiryTime := time.UnixMilli(int64(z.Score))
			markers = append(markers, networkPeerDisconnectMarker(clientId, expiryTime))
			pipe.HDel(ctx, networkPeerMetaKey(networkId), member)
			pipe.ZRem(ctx, networkPeerConnectedKey(networkId), member)
			pipe.ZAdd(ctx, networkPeerDisconnectedKey(networkId), redis.Z{
				Score:  z.Score,
				Member: member,
			})
		}
		_, err := pipe.Exec(ctx)
		if err != nil {
			panic(err)
		}

		publishNetworkPeerEvent(ctx, r, networkId, NetworkPeerEventTypeRemoved, markers)
	}

	// age out old disconnect markers
	err = r.ZRemRangeByScore(
		ctx,
		networkPeerDisconnectedKey(networkId),
		"-inf",
		strconv.FormatInt(nowMs-NetworkPeerDisconnectedWindow.Milliseconds(), 10),
	).Err()
	if err != nil {
		panic(err)
	}
}

// GetNetworkPeers returns the current peers of a network: connected peers
// with metadata plus disconnect markers within the disconnected window.
// Connected entries whose expiry passed but are not yet pruned are reported
// as disconnect markers at their expiry time.
func GetNetworkPeers(ctx context.Context, networkId server.Id) (eventId int64, peers []*NetworkPeer) {
	server.Redis(ctx, func(r server.RedisClient) {
		nowMs := server.NowUtc().UnixMilli()
		windowStartMs := nowMs - NetworkPeerDisconnectedWindow.Milliseconds()

		pipe := r.TxPipeline()
		metaCmd := pipe.HGetAll(ctx, networkPeerMetaKey(networkId))
		connectedCmd := pipe.ZRangeByScoreWithScores(ctx, networkPeerConnectedKey(networkId), &redis.ZRangeBy{
			Min: "-inf",
			Max: "+inf",
		})
		disconnectedCmd := pipe.ZRangeByScoreWithScores(ctx, networkPeerDisconnectedKey(networkId), &redis.ZRangeBy{
			Min: strconv.FormatInt(windowStartMs, 10),
			Max: "+inf",
		})
		eventIdCmd := pipe.Get(ctx, networkPeerEventIdKey(networkId))
		_, err := pipe.Exec(ctx)
		if err != nil && err != server.RedisNil {
			panic(err)
		}

		metas, err := metaCmd.Result()
		if err != nil && err != server.RedisNil {
			panic(err)
		}
		connected, err := connectedCmd.Result()
		if err != nil && err != server.RedisNil {
			panic(err)
		}
		disconnected, err := disconnectedCmd.Result()
		if err != nil && err != server.RedisNil {
			panic(err)
		}
		eventId, _ = eventIdCmd.Int64()

		for _, z := range connected {
			member := z.Member.(string)
			clientId := server.Id([]byte(member))
			expiryMs := int64(z.Score)
			if nowMs < expiryMs {
				meta, _ := loadNetworkPeerMeta([]byte(metas[member]))
				if meta != nil {
					peers = append(peers, meta.Peer)
				}
			} else if windowStartMs < expiryMs {
				// expired but not yet pruned
				peers = append(peers, networkPeerDisconnectMarker(clientId, time.UnixMilli(expiryMs)))
			}
		}
		for _, z := range disconnected {
			member := z.Member.(string)
			clientId := server.Id([]byte(member))
			peers = append(peers, networkPeerDisconnectMarker(clientId, time.UnixMilli(int64(z.Score))))
		}
	})
	return
}

func GetNetworkPeerEventId(ctx context.Context, networkId server.Id) (eventId int64) {
	server.Redis(ctx, func(r server.RedisClient) {
		eventId_, err := r.Get(ctx, networkPeerEventIdKey(networkId)).Int64()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		eventId = eventId_
	})
	return
}

type NetworkPeerListener struct {
	ctx         context.Context
	cancel      context.CancelFunc
	networkId   server.Id
	callback    func(*NetworkPeerEvent)
	pollTimeout time.Duration
}

func NewNetworkPeerListener(
	ctx context.Context,
	networkId server.Id,
	callback func(*NetworkPeerEvent),
	pollTimeout time.Duration,
) *NetworkPeerListener {
	cancelCtx, cancel := context.WithCancel(ctx)

	npl := &NetworkPeerListener{
		ctx:         cancelCtx,
		cancel:      cancel,
		networkId:   networkId,
		callback:    callback,
		pollTimeout: pollTimeout,
	}
	go server.HandleError(npl.run)
	return npl
}

func (self *NetworkPeerListener) run() {
	defer self.cancel()

	messages, unsub := server.Subscribe(self.ctx, networkPeerEventsKey(self.networkId))
	defer unsub()

	// the last processed event id. Event ids start at 1 and only move
	// backward when the registry is flushed and rebuilt, which resyncs below.
	var eventId int64
	synced := false

	reset := func() {
		resetEventId, resetPeers := GetNetworkPeers(self.ctx, self.networkId)
		if !synced || resetEventId != eventId {
			synced = true
			eventId = resetEventId

			resetEvent := &NetworkPeerEvent{
				NetworkPeerEventType: NetworkPeerEventTypeReset,
				EventId:              resetEventId,
				Peers:                resetPeers,
			}
			self.callback(resetEvent)
		}
	}

	for {
		select {
		case <-self.ctx.Done():
			return
		case m := <-messages:
			switch v := m.(type) {
			case server.RedisSubscription:
				switch v.Kind {
				case "subscribe", "ssubscribe":
					reset()
				}
			case server.RedisMessage:
				buf := bytes.NewBuffer([]byte(v.Payload))
				decoder := gob.NewDecoder(buf)
				var event NetworkPeerEvent
				err := decoder.Decode(&event)
				if err == nil {
					if eventId+1 == event.EventId {
						eventId = event.EventId
						self.callback(&event)
					} else {
						// a gap in delivery, or an event id at or below the
						// last processed (the counter moved backward, e.g. a
						// flushed registry): resync
						reset()
					}
				}
			}
		case <-time.After(self.pollTimeout):
			// no event, poll in case events are not being delivered
			// or the counter moved backward
			if GetNetworkPeerEventId(self.ctx, self.networkId) != eventId {
				reset()
			}
		}
	}
}

func (self *NetworkPeerListener) Close() {
	self.cancel()
}

type NetworkPeersResult struct {
	// connected peers
	Peers []*NetworkPeer `json:"peers"`
	// disconnect markers within the disconnected window
	Disconnected      []*NetworkPeer `json:"disconnected,omitempty"`
	DisconnectedCount int            `json:"disconnected_count"`

	Error *NetworkPeersError `json:"error,omitempty"`
}

type NetworkPeersError struct {
	Message string `json:"message"`
}

// GetNetworkPeersForSession backs the fast peer discovery api. Allowed for
// network-level non-guest sessions (all peers) and top-level client sessions
// (peers excluding self). Reads only the redis peer registry.
func GetNetworkPeersForSession(session *session.ClientSession) (*NetworkPeersResult, error) {
	if session.ByJwt.GuestMode {
		return &NetworkPeersResult{
			Error: &NetworkPeersError{
				Message: "Not allowed.",
			},
		}, nil
	}

	var selfClientId *server.Id
	if session.ByJwt.ClientId != nil {
		// only top-level clients have peers
		clientId := *session.ByJwt.ClientId
		topLevel := false
		server.Db(session.Ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				session.Ctx,
				`
					SELECT source_client_id FROM network_client
					WHERE
						client_id = $1 AND
						network_id = $2 AND
						active = true
				`,
				clientId,
				session.ByJwt.NetworkId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					var sourceClientId *server.Id
					server.Raise(result.Scan(&sourceClientId))
					topLevel = sourceClientId == nil
				}
			})
		})
		if !topLevel {
			return &NetworkPeersResult{
				Error: &NetworkPeersError{
					Message: "Not allowed.",
				},
			}, nil
		}
		selfClientId = &clientId
	}

	_, allPeers := GetNetworkPeers(session.Ctx, session.ByJwt.NetworkId)

	result := &NetworkPeersResult{
		Peers: []*NetworkPeer{},
	}
	for _, peer := range allPeers {
		if selfClientId != nil && peer.ClientId == *selfClientId {
			continue
		}
		if peer.DisconnectTime != nil {
			result.Disconnected = append(result.Disconnected, peer)
		} else {
			result.Peers = append(result.Peers, peer)
		}
	}
	result.DisconnectedCount = len(result.Disconnected)

	return result, nil
}
