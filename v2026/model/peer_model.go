package model

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	mathrand "math/rand"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// the network peer registry stores the set of connected top-level clients per
// network (clients with no `source_client_id`) and identity metadata for each:
// enabled provide modes, principal, and roles. The connection announce
// registers the client once it survives the announce window, the resident
// heartbeats it on the resident poll, and the resident removes it on close
// (residentId-guarded, like the resident registry).
//
// Change notification is dirty-counter + poll (PEERS2.md): every visible
// mutation bumps the per-network version counter, and each listener polls the
// counter at its own rate, full-reading only on a mismatch. v1's per-event
// sharded-pubsub delivery (one subscription per connected client, fanout to
// every device of the network per change) melted the redis cluster on
// 2026-07-15 — see FOLLOWUP.md "Network peers pubsub" for the record.

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

// the per-network version counter (PEERS2.md): INCR'd on every visible
// registry change, polled by readers. There is no events channel in v2.
func networkPeerEventIdKey(networkId server.Id) string {
	return fmt.Sprintf("{np_%s}eid", networkId)
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

// bumpNetworkPeerVersion marks the network's peer registry as changed
// (PEERS2.md): readers poll this per-network counter at their own rate and
// full-read on any mismatch. v1 published the change over sharded pubsub
// here; per-event delivery to every device of the network is exactly the
// fanout that melted the cluster on 2026-07-15, so v2 delivers nothing —
// a change costs one INCR, and read cost is demand-driven at the readers.
func bumpNetworkPeerVersion(
	ctx context.Context,
	r server.RedisClient,
	networkId server.Id,
) {
	pipe := r.TxPipeline()
	pipe.Incr(ctx, networkPeerEventIdKey(networkId))
	pipe.Expire(ctx, networkPeerEventIdKey(networkId), networkPeerKeyTtl)
	_, err := pipe.Exec(ctx)
	if err != nil {
		panic(err)
	}
}

// how long a `NetworkPeersEnabled` decision is cached per network. The
// decision is derived from the active top-level client count, which changes
// only on client create and remove — and create cannot push a network over
// the limit (`AuthNetworkClient` rejects at the limit) — so the only
// transition a stale entry can delay is an over-limit network gaining peers
// after shrinking under the limit.
const networkPeersEnabledTtl = 5 * time.Minute

type networkPeersEnabledEntry struct {
	enabled    bool
	expireTime time.Time
}

// a process-local ttl cache of the `NetworkPeersEnabled` decision per
// network, so that resident creation does not count clients on every connect
type peersEnabledCache struct {
	lock          sync.Mutex
	entries       map[server.Id]networkPeersEnabledEntry
	nextSweepTime time.Time
}

func (self *peersEnabledCache) Get(networkId server.Id) (enabled bool, ok bool) {
	self.lock.Lock()
	defer self.lock.Unlock()
	entry, ok := self.entries[networkId]
	if !ok {
		return false, false
	}
	if entry.expireTime.Before(time.Now()) {
		delete(self.entries, networkId)
		return false, false
	}
	return entry.enabled, true
}

func (self *peersEnabledCache) Put(networkId server.Id, enabled bool) {
	self.lock.Lock()
	defer self.lock.Unlock()
	now := time.Now()
	// sweep expired entries at most once per ttl so the map stays bounded by
	// the networks seen in the last ttl
	if self.nextSweepTime.Before(now) {
		self.nextSweepTime = now.Add(networkPeersEnabledTtl)
		for networkId, entry := range self.entries {
			if entry.expireTime.Before(now) {
				delete(self.entries, networkId)
			}
		}
	}
	self.entries[networkId] = networkPeersEnabledEntry{
		enabled:    enabled,
		expireTime: now.Add(networkPeersEnabledTtl),
	}
}

func (self *peersEnabledCache) Clear() {
	self.lock.Lock()
	defer self.lock.Unlock()
	clear(self.entries)
}

var networkPeersEnabledCache = &peersEnabledCache{
	entries: map[server.Id]networkPeersEnabledEntry{},
}

// Testing_ClearNetworkPeersEnabledCache resets the process-local
// `NetworkPeersEnabled` cache, so a test can observe a count transition
// within the cache ttl
func Testing_ClearNetworkPeersEnabledCache() {
	networkPeersEnabledCache.Clear()
}

// NetworkPeersEnabled returns whether the network is within the top-level
// client limit. Networks above the limit (created before the limit) do not
// get peer registrations or subscriptions, since the peer replay and event
// fan-out scale with the number of connected top-level clients.
// The decision is cached per network for `networkPeersEnabledTtl`, and the
// count scan is bounded at the limit since only the threshold matters.
//
// Gated by enforce_concurrent_clients: while the concurrent-client rollout is
// dark this always returns true (every network gets peer registrations,
// regardless of size) with no db lookup -- the cap is not enforced (see
// pro.yml). Pro() is parsed once, so the gate cannot change under the cache
// within a process.
func NetworkPeersEnabled(ctx context.Context, networkId server.Id) bool {
	if !Pro().EnforceConcurrentClients {
		return true
	}
	if enabled, ok := networkPeersEnabledCache.Get(networkId); ok {
		return enabled
	}
	enabled := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT COUNT(*) AS top_level_client_count
				FROM (
					SELECT 1
					FROM network_client
					WHERE
						network_id = $1 AND
						active = true AND
						source_client_id IS NULL
					LIMIT $2
				) t
			`,
			networkId,
			LimitTopLevelClientIdsPerNetwork+1,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				topLevelClientCount := 0
				server.Raise(result.Scan(&topLevelClientCount))
				enabled = topLevelClientCount <= LimitTopLevelClientIdsPerNetwork
			}
		})
	})
	networkPeersEnabledCache.Put(networkId, enabled)
	return enabled
}

// GetNetworkPeerProfile loads the network, top-level status, category, and
// identity metadata used to register a client in the peer registry, plus
// whether the network is enabled for peers (`NetworkPeersEnabled`, typically
// resolved from the process-local cache so no additional query is made).
// `peer` is nil when the client does not exist or is not active. `category` is
// proxy when the client has a hosted proxy device (a proxy_device_config row).
// `peersEnabled` is false whenever the client is not an active top-level client.
func GetNetworkPeerProfile(ctx context.Context, clientId server.Id) (networkId server.Id, topLevel bool, category NetworkPeerCategory, peer *NetworkPeer, peersEnabled bool) {
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

	if topLevel && peer != nil {
		peersEnabled = NetworkPeersEnabled(ctx, networkId)
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

		bumpNetworkPeerVersion(ctx, r, networkId)

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

		bumpNetworkPeerVersion(ctx, r, networkId)
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

// isPublicProvider reports whether a peer is running as a public provider, i.e.
// it offers BOTH public and stream provide modes. Public providers contribute
// capacity to the network rather than consuming it, so they are exempt from the
// connected top-level client limit.
func isPublicProvider(peer *NetworkPeer) bool {
	if peer == nil {
		return false
	}
	public := false
	stream := false
	for _, provideMode := range peer.ProvideModes {
		switch provideMode {
		case ProvideModePublic:
			public = true
		case ProvideModeStream:
			stream = true
		}
	}
	return public && stream
}

// GetNetworkEnforceableConnectedCount returns the number of connected top-level
// clients that count toward a network's concurrent-client limit: the same set as
// GetNetworkConnectedCount, minus any client running as a public provider
// (public + stream provide mode), which is exempt.
//
// This is the count to compare against model.Pro().ConcurrentClientsExceeded.
// Hosted proxy clients never register provide modes, so they can never be exempt
// and always count.
func GetNetworkEnforceableConnectedCount(ctx context.Context, networkId server.Id) (count int) {
	server.Redis(ctx, func(r server.RedisClient) {
		nowMs := server.NowUtc().UnixMilli()
		// count only entries whose expiry is still in the future
		liveMin := strconv.FormatInt(nowMs+1, 10)

		pipe := r.TxPipeline()
		metaCmd := pipe.HGetAll(ctx, networkPeerMetaKey(networkId))
		connectedCmd := pipe.ZRangeByScore(ctx, networkPeerConnectedKey(networkId), &redis.ZRangeBy{
			Min: liveMin,
			Max: "+inf",
		})
		proxyCmd := pipe.ZCount(ctx, networkPeerConnectedProxyKey(networkId), liveMin, "+inf")
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

		count = int(proxyCmd.Val())
		for _, member := range connected {
			meta, _ := loadNetworkPeerMeta([]byte(metas[member]))
			if meta != nil && isPublicProvider(meta.Peer) {
				// exempt: this client provides for the network
				continue
			}
			count += 1
		}
	})
	return
}

// isNetworkPeerConnected reports whether a client is currently registered as a
// connected top-level client (ordinary or hosted proxy) whose entry has not yet
// expired.
func isNetworkPeerConnected(ctx context.Context, networkId server.Id, clientId server.Id) (connected bool) {
	member := string(clientId.Bytes())

	server.Redis(ctx, func(r server.RedisClient) {
		nowMs := server.NowUtc().UnixMilli()

		pipe := r.TxPipeline()
		clientCmd := pipe.ZScore(ctx, networkPeerConnectedKey(networkId), member)
		proxyCmd := pipe.ZScore(ctx, networkPeerConnectedProxyKey(networkId), member)
		_, err := pipe.Exec(ctx)
		if err != nil && err != server.RedisNil {
			panic(err)
		}

		for _, cmd := range []*redis.FloatCmd{clientCmd, proxyCmd} {
			if expiryMs, err := cmd.Result(); err == nil && nowMs < int64(expiryMs) {
				connected = true
				return
			}
		}
	})
	return
}

// NetworkConcurrentClientsExceeded reports whether a network already has its plan's
// full complement of connected top-level clients, i.e. there is no room for another.
//
// While enforcement is dark this returns false IMMEDIATELY, with no redis and no db
// lookup, so shipping the gate costs nothing on the auth hot path -- the cost only
// arrives with the rollout. Public providers are exempt from the count, and Pro is
// read live (never from the jwt's stale claim). This is the client-creation gate;
// CanConnectNetworkPeer is the connection-activation gate.
func NetworkConcurrentClientsExceeded(ctx context.Context, networkId server.Id) bool {
	// dark, or no pro.yml at all -> no limit, and no i/o to find that out
	if !Pro().EnforceConcurrentClients {
		return false
	}

	pro := IsProNetwork(ctx, networkId)
	connectedCount := GetNetworkEnforceableConnectedCount(ctx, networkId)
	return Pro().ConcurrentClientsExceeded(pro, connectedCount)
}

// CanConnectNetworkPeer reports whether `clientId` may become a connected
// top-level client of its network without exceeding the network's plan limit on
// concurrent connected clients (pro.yml concurrent_clients).
//
// It is always true when:
//   - enforcement is dark (pro.yml enforce_concurrent_clients = false);
//   - the client is not a top-level client — only top-level clients count;
//   - the client runs as a public provider (public + stream provide mode), which
//     adds capacity to the network rather than consuming it, and so is exempt;
//   - the client is already registered as connected — a re-nomination (e.g.
//     resident replacement) is not a new connection and is already counted.
//
// Otherwise the network's enforceable connected count is compared against its
// tier limit. This is the connection-activation gate; AuthNetworkClient applies
// the same limit at client creation. It fails open (allows) for an unknown
// client.
func CanConnectNetworkPeer(ctx context.Context, clientId server.Id) bool {
	// dark, or no pro.yml at all -> allowed, and no i/o to find that out
	if !Pro().EnforceConcurrentClients {
		return true
	}

	networkId, topLevel, _, peer, _ := GetNetworkPeerProfile(ctx, clientId)
	if !topLevel {
		return true
	}
	if isPublicProvider(peer) {
		return true
	}
	if isNetworkPeerConnected(ctx, networkId, clientId) {
		return true
	}

	pro := IsPro(ctx, &networkId)
	connectedCount := GetNetworkEnforceableConnectedCount(ctx, networkId)
	return !Pro().ConcurrentClientsExceeded(pro, connectedCount)
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

		bumpNetworkPeerVersion(ctx, r, *networkId)
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
		// the ZAdd above can create the disconnected key (Expire in add/refresh
		// is a no-op on a missing key), which would otherwise live ttl-less
		pipe.Expire(ctx, networkPeerDisconnectedKey(networkId), networkPeerKeyTtl)
		_, err := pipe.Exec(ctx)
		if err != nil {
			panic(err)
		}

		bumpNetworkPeerVersion(ctx, r, networkId)
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
	ctx           context.Context
	cancel        context.CancelFunc
	networkId     server.Id
	callback      func(*NetworkPeerEvent)
	pollInterval  time.Duration
	fullReadEvery int
}

// NewNetworkPeerListener polls the network's peer version counter every
// `pollInterval` (jittered ±20%) and emits a Reset event with the full peer
// list whenever the counter moved (PEERS2.md). Every `fullReadEvery`-th tick
// full-reads unconditionally as insurance against a missed bump. There are
// no subscriptions and no standing connections: the reader polls at its own
// rate, and a slow or failing reader accumulates no state anywhere.
func NewNetworkPeerListener(
	ctx context.Context,
	networkId server.Id,
	callback func(*NetworkPeerEvent),
	pollInterval time.Duration,
	fullReadEvery int,
) *NetworkPeerListener {
	cancelCtx, cancel := context.WithCancel(ctx)

	npl := &NetworkPeerListener{
		ctx:           cancelCtx,
		cancel:        cancel,
		networkId:     networkId,
		callback:      callback,
		pollInterval:  pollInterval,
		fullReadEvery: fullReadEvery,
	}
	go server.HandleError(npl.run)
	return npl
}

func (self *NetworkPeerListener) run() {
	defer self.cancel()

	// the last synced version. `!=` (never `<`): the counter moves backward
	// when the registry is flushed, ttl-expires, or is rebuilt — any mismatch
	// resyncs from a full read.
	var eventId int64
	synced := false

	// full-read and deliver the current snapshot. `force` delivers even when
	// the version matches the last synced value — used by the insurance tick,
	// which must self-heal drift the version comparison cannot see (a missed
	// bump, or a flushed counter that restarts at a value already synced).
	reset := func(force bool) {
		resetEventId, resetPeers := GetNetworkPeers(self.ctx, self.networkId)
		if !synced || force || resetEventId != eventId {
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

	consecutiveErrors := 0
	for tick := 0; ; tick += 1 {
		// ±20% jitter spreads fleet polls; error backoff (up to 4x) keeps a
		// sick slot from being hammered
		interval := self.pollInterval + time.Duration((mathrand.Float64()*0.4-0.2)*float64(self.pollInterval))
		interval <<= consecutiveErrors

		select {
		case <-self.ctx.Done():
			return
		case <-time.After(interval):
		}

		// contain redis panics to the tick: a failed poll must never kill the
		// listener (2026-07-15: panicked listeners died permanently and their
		// clients silently stopped receiving peer updates)
		if r := server.HandleError(func() {
			insurance := 0 < self.fullReadEvery && tick%self.fullReadEvery == 0
			if !synced || insurance {
				reset(true)
			} else if GetNetworkPeerEventId(self.ctx, self.networkId) != eventId {
				reset(false)
			}
		}); r != nil {
			consecutiveErrors = min(consecutiveErrors+1, 2)
		} else {
			consecutiveErrors = 0
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
