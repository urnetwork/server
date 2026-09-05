package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/json"
	"fmt"
	mathrand "math/rand"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
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

// PEERSSTREAMS2.md: per-member peer key. Each registered peer gets its own
// string key carrying the gob meta, TTL'd to the registration ttl, so redis
// keyspace notifications announce add / metadata change (`set`), clean remove
// (`del`), and silent death (`expired`) per peer. Heartbeat refresh extends
// the TTL with EXPIRE, whose `expire` notification readers ignore — a
// refresh is not a visible change. The meta hash and zsets remain the
// read/index model; these keys exist for the notification stream.
func networkPeerMemberKey(networkId server.Id, clientId server.Id) string {
	return fmt.Sprintf("{np_%s}p:%s", networkId, clientId)
}

// Per-client mutation fence for the denormalized peer registration. Add reads
// canonical provide modes only after planting this key; every registry
// mutation compares or advances it in the same Redis-slot transition. Keeping
// it as a TTL'd string, rather than a field in the per-network meta hash,
// bounds state for clients that leave an otherwise-active network.
func networkPeerMutationVersionKey(networkId server.Id, clientId server.Id) string {
	return fmt.Sprintf("{np_%s}mv:%s", networkId, clientId)
}

// A short-lived receipt makes one logical mutation replay-safe across both
// go-redis command retries and the server Redis wrapper's callback retries.
// The operation id is stable across optimistic-state retries.
func networkPeerMutationReceiptKey(networkId server.Id, operationId server.Id) string {
	return fmt.Sprintf("{np_%s}mr:%s", networkId, operationId)
}

// NetworkPeerKeyEventPattern is the psubscribe pattern for the keyspace
// channels of ALL per-member peer keys on a db. One broad pattern per
// subscriber connection keeps the redis-side pattern-match cost O(1) per
// event; routing by network happens in-process (PEERSSTREAMS2.md §5.1).
func NetworkPeerKeyEventPattern(db int) string {
	return fmt.Sprintf("__keyspace@%d__:{np_*}p:*", db)
}

// ParseNetworkPeerKeyEvent extracts the network and client ids from a
// keyspace channel name matching `NetworkPeerKeyEventPattern`.
func ParseNetworkPeerKeyEvent(channel string) (networkId server.Id, clientId server.Id, ok bool) {
	i := strings.Index(channel, ":{np_")
	if i < 0 {
		return
	}
	rest := channel[i+len(":{np_"):]
	j := strings.Index(rest, "}p:")
	if j < 0 {
		return
	}
	networkId, err := server.ParseId(rest[:j])
	if err != nil {
		return
	}
	clientId, err = server.ParseId(rest[j+len("}p:"):])
	if err != nil {
		return
	}
	ok = true
	return
}

// GetNetworkPeerMember reads one peer's registration (the key-event delta
// read): the peer when registered, else nil.
func GetNetworkPeerMember(ctx context.Context, networkId server.Id, clientId server.Id) (peer *NetworkPeer) {
	member := string(clientId.Bytes())
	server.Redis(ctx, func(r server.RedisClient) {
		metaBytes, err := r.HGet(ctx, networkPeerMetaKey(networkId), member).Bytes()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		meta, _ := loadNetworkPeerMeta(metaBytes)
		if meta != nil {
			peer = meta.Peer
		}
	})
	return
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

// how long a `NetworkPeersEnabled` decision is cached per network. The
// decision is derived from the active top-level client count, which changes
// only on client create and remove. While the creation cap is dark
// (enforce_concurrent_clients = false) a network can cross the limit in either
// direction, so a stale entry can delay a transition by up to the ttl: an
// under-limit network keeps peers for up to ttl after growing past the limit
// (bounded extra fan-out on the network's shard), and an over-limit network
// gains peers up to ttl after shrinking below it. Both are benign.
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

// networkPeersRecentAuthWindow bounds the NetworkPeersEnabled count to the
// network's recently-active top-level clients. auth_time refreshes on both auth
// AND connect (see ConnectNetworkClient), so "authed within the window" is the
// set of clients that have recently connected and could be polling the peer
// zsets — the set that actually drives the shard fan-out. It deliberately
// EXCLUDES dormant-but-active clients: a top-level client is retained up to
// TopLevelClientIdleExpiration (30 days) after its last auth/connect before the
// reaper marks it inactive, so a network can carry thousands of active rows that
// have not connected in months (identity churn — a fresh device_id per login —
// or leaked proxy-watchdog clients) while only a handful are live. Those dormant
// rows register no peers and drive no cost, so they must not trip the valve.
const networkPeersRecentAuthWindow = 14 * 24 * time.Hour

// NetworkPeersEnabled returns whether the network is within the top-level client
// limit (LimitTopLevelClientIdsPerNetwork), counting only RECENTLY-ACTIVE
// top-level clients (auth_time within networkPeersRecentAuthWindow). Networks
// above the limit (genuinely large concurrent fleets) get NO peer registrations
// or subscriptions: the v2 poll architecture has each connected top-level client
// full-read the network's connected/disconnected zsets + meta hash on every
// change, all on the single shard that owns the hash-tagged `{np_<networkId>}`
// keys, so the per-shard cost scales with the connected top-level client count —
// unbounded, it is O(size^2) fan-out on one shard (2026-07-17: an ~8,900-client
// network drove its shard to ~1,700 full-reads/sec and starved every other key
// on it).
//
// Counting ALL active top-level clients (the original valve) wrongly disabled
// the feature for any network that merely ACCUMULATED dormant top-level clients
// under the 30-day retention window, even when only two devices were ever
// connected. Bounding to the recent-auth working set fixes that without
// deactivating anything: dormant rows stay valid clients, they just no longer
// count against the peer valve. The window is a conservative over-count of the
// truly-connected set (it also includes clients that disconnected within the
// window), so it still errs toward disabling for genuinely busy networks.
//
// This valve is enforced INDEPENDENTLY of enforce_concurrent_clients: it only
// removes the peer feature. The connection and creation caps
// (NetworkConcurrentClientsExceeded, CanConnectNetworkPeer, AuthNetworkClient)
// stay separately gated by enforce_concurrent_clients, so an over-limit network
// still connects and carries traffic exactly as before — it just polls no peer
// list. The decision is cached per network for `networkPeersEnabledTtl`, and the
// count scan is bounded at the limit since only the threshold matters.
func NetworkPeersEnabled(ctx context.Context, networkId server.Id) bool {
	if enabled, ok := networkPeersEnabledCache.Get(networkId); ok {
		return enabled
	}
	enabled := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT COUNT(*) AS recent_top_level_client_count
				FROM (
					SELECT 1
					FROM network_client
					WHERE
						network_id = $1 AND
						active = true AND
						source_client_id IS NULL AND
						auth_time > $3
					LIMIT $2
				) t
			`,
			networkId,
			LimitTopLevelClientIdsPerNetwork+1,
			server.NowUtc().Add(-networkPeersRecentAuthWindow),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				recentTopLevelClientCount := 0
				server.Raise(result.Scan(&recentTopLevelClientCount))
				enabled = recentTopLevelClientCount <= LimitTopLevelClientIdsPerNetwork
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

const (
	networkPeerMutationRetryDelay  = 5 * time.Millisecond
	networkPeerMutationMaxAttempts = 16
	networkPeerMutationReceiptTtl  = 5 * time.Minute
)

// One optimistic read used by the exact-value Lua comparisons. A pipeline read
// may race another writer, but no torn pair can pass the later comparison.
type networkPeerMutationState struct {
	metaBytes          []byte
	hasMeta            bool
	mutationVersion    int64
	hasMutationVersion bool
}

// Returns an independent profile for registry storage. In particular, an Add
// retry must not rewrite the caller's stale ProvideModes slice while replacing
// it with the canonical sorted value.
func cloneNetworkPeer(peer *NetworkPeer) *NetworkPeer {
	clonedPeer := *peer
	clonedPeer.ProvideModes = slices.Clone(peer.ProvideModes)
	clonedPeer.Roles = slices.Clone(peer.Roles)
	if peer.DisconnectTime != nil {
		disconnectTime := *peer.DisconnectTime
		clonedPeer.DisconnectTime = &disconnectTime
	}
	return &clonedPeer
}

// Captures the two values every ownership-sensitive mutation compares in its
// Lua transition. Missing version is valid for registrations written by a
// pre-fence binary and is distinct from version zero.
func getNetworkPeerMutationState(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
) (state networkPeerMutationState) {
	member := string(clientId.Bytes())
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.Pipeline()
		metaCmd := pipe.HGet(ctx, networkPeerMetaKey(networkId), member)
		mutationVersionCmd := pipe.Get(ctx, networkPeerMutationVersionKey(networkId, clientId))
		_, err := pipe.Exec(ctx)
		if err != nil && err != server.RedisNil {
			panic(err)
		}

		metaBytes, err := metaCmd.Bytes()
		if err != nil && err != server.RedisNil {
			panic(err)
		}
		if err == nil {
			state.metaBytes = metaBytes
			state.hasMeta = true
		}

		mutationVersion, err := mutationVersionCmd.Int64()
		if err != nil && err != server.RedisNil {
			panic(err)
		}
		if err == nil {
			state.mutationVersion = mutationVersion
			state.hasMutationVersion = true
		}
	})
	return
}

// Rate-limits a conflicting mutation retry and makes cancellation one bound
// when a peer is changing continuously.
func waitNetworkPeerMutationRetry(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(networkPeerMutationRetryDelay):
		return true
	}
}

// Runs a conflicting optimistic mutation only a finite number of times. The
// attempt callback returns true for every terminal outcome, including a
// guarded no-op; false means its exact-value comparison lost a race.
func retryNetworkPeerMutation(ctx context.Context, attempt func() bool) bool {
	for attemptIndex := 0; attemptIndex < networkPeerMutationMaxAttempts; attemptIndex++ {
		if attempt() {
			return true
		}
		if attemptIndex+1 == networkPeerMutationMaxAttempts || !waitNetworkPeerMutationRetry(ctx) {
			return false
		}
	}
	return false
}

// Plants the intent observed by provide-mode updates before Add reads the
// canonical modes. Its TTL is refreshed so a slow but live registration does
// not lose the fence between the read and commit.
func prepareNetworkPeerMutation(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
) (mutationVersion int64) {
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		mutationVersion, err = r.Eval(
			ctx,
			`
			local mutation_version_key = KEYS[1]
			local key_ttl_seconds = ARGV[1]

			local mutation_version = redis.call('GET', mutation_version_key)
			if mutation_version == false then
				redis.call('SET', mutation_version_key, 0, 'EX', key_ttl_seconds)
				return 0
			end
			redis.call('EXPIRE', mutation_version_key, key_ttl_seconds)
			return tonumber(mutation_version)
			`,
			[]string{networkPeerMutationVersionKey(networkId, clientId)},
			int64(networkPeerKeyTtl/time.Second),
		).Int64()
		if err != nil {
			panic(err)
		}
	})
	return
}

// Commits one registration only if no provide update or lifecycle mutation
// occurred since canonical modes were loaded.
func addNetworkPeerAtMutationVersion(
	ctx context.Context,
	networkId server.Id,
	peer *NetworkPeer,
	residentId server.Id,
	ttl time.Duration,
	expectedMutationVersion int64,
	operationId server.Id,
	pruneOperationId server.Id,
) (added bool) {
	meta := &networkPeerMeta{
		Peer:       peer,
		ResidentId: residentId,
	}
	metaBytes := meta.Bytes()
	member := string(peer.ClientId.Bytes())
	expiryMs := server.NowUtc().Add(ttl).UnixMilli()

	server.Redis(ctx, func(r server.RedisClient) {
		addedInt, err := r.Eval(
			ctx,
			`
			local meta_key = KEYS[1]
			local member_key = KEYS[2]
			local connected_key = KEYS[3]
			local disconnected_key = KEYS[4]
			local event_id_key = KEYS[5]
			local mutation_version_key = KEYS[6]
			local mutation_receipt_key = KEYS[7]

			local member = ARGV[1]
			local expected_mutation_version = ARGV[2]
			local meta = ARGV[3]
			local expiry_ms = ARGV[4]
			local member_ttl_ms = ARGV[5]
			local key_ttl_seconds = ARGV[6]
			local receipt_ttl_seconds = ARGV[7]

			local previous_result = redis.call('GET', mutation_receipt_key)
			if previous_result ~= false then
				return tonumber(previous_result)
			end

			if redis.call('GET', mutation_version_key) ~= expected_mutation_version then
				return 0
			end

			redis.call('HSET', meta_key, member, meta)
			redis.call('SET', member_key, meta, 'PX', member_ttl_ms)
			redis.call('ZADD', connected_key, expiry_ms, member)
			redis.call('ZREM', disconnected_key, member)
			redis.call('INCR', mutation_version_key)
			redis.call('INCR', event_id_key)

			redis.call('EXPIRE', meta_key, key_ttl_seconds)
			redis.call('EXPIRE', connected_key, key_ttl_seconds)
			redis.call('EXPIRE', disconnected_key, key_ttl_seconds)
			redis.call('EXPIRE', mutation_version_key, key_ttl_seconds)
			redis.call('EXPIRE', event_id_key, key_ttl_seconds)
			redis.call('SET', mutation_receipt_key, 1, 'EX', receipt_ttl_seconds)
			return 1
			`,
			[]string{
				networkPeerMetaKey(networkId),
				networkPeerMemberKey(networkId, peer.ClientId),
				networkPeerConnectedKey(networkId),
				networkPeerDisconnectedKey(networkId),
				networkPeerEventIdKey(networkId),
				networkPeerMutationVersionKey(networkId, peer.ClientId),
				networkPeerMutationReceiptKey(networkId, operationId),
			},
			member,
			expectedMutationVersion,
			metaBytes,
			expiryMs,
			ttl.Milliseconds(),
			int64(networkPeerKeyTtl/time.Second),
			int64(networkPeerMutationReceiptTtl/time.Second),
		).Int()
		if err != nil {
			panic(err)
		}
		added = addedInt == 1
		if added {
			pruneNetworkPeers(ctx, r, networkId, pruneOperationId)
		}
	})
	return
}

// The loader boundary keeps the ordering test deterministic without a
// package-global hook. Production supplies GetProvideModes; a test can stop
// the first load after the intent fence has been planted.
func addNetworkPeerWithProvideModesLoader(
	ctx context.Context,
	networkId server.Id,
	peer *NetworkPeer,
	residentId server.Id,
	ttl time.Duration,
	loadProvideModes func() (map[ProvideMode]bool, error),
) bool {
	operationId := server.NewId()
	pruneOperationId := server.NewId()
	return retryNetworkPeerMutation(ctx, func() bool {
		mutationVersion := prepareNetworkPeerMutation(ctx, networkId, peer.ClientId)
		provideModes, err := loadProvideModes()
		server.Raise(err)

		registeredPeer := cloneNetworkPeer(peer)
		registeredPeer.ProvideModes = sortedProvideModesList(provideModes)
		if addNetworkPeerAtMutationVersion(
			ctx,
			networkId,
			registeredPeer,
			residentId,
			ttl,
			mutationVersion,
			operationId,
			pruneOperationId,
		) {
			return true
		}
		return false
	})
}

// Registers a connected top-level client and publishes an updated event. The
// provide modes in `peer` are only a profile snapshot: registration reloads
// their canonical value behind the mutation fence so a concurrent SetProvide
// cannot be overwritten by a stale announce.
func AddNetworkPeer(
	ctx context.Context,
	networkId server.Id,
	peer *NetworkPeer,
	residentId server.Id,
	ttl time.Duration,
) {
	addNetworkPeerWithProvideModesLoader(ctx, networkId, peer, residentId, ttl, func() (map[ProvideMode]bool, error) {
		return GetProvideModes(ctx, peer.ClientId)
	})
}

// Extends one exact registration without producing a visible event. A missing
// member key is a lost registration, not permission to recreate metadata read
// before a concurrent replacement.
func refreshNetworkPeerAtMutationState(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	ttl time.Duration,
	state networkPeerMutationState,
	pruneOperationId server.Id,
) (refreshed bool, conflict bool) {
	member := string(clientId.Bytes())
	expiryMs := server.NowUtc().Add(ttl).UnixMilli()
	hasMutationVersionInt := 0
	if state.hasMutationVersion {
		hasMutationVersionInt = 1
	}

	server.Redis(ctx, func(r server.RedisClient) {
		refreshedInt, err := r.Eval(
			ctx,
			`
			local meta_key = KEYS[1]
			local member_key = KEYS[2]
			local connected_key = KEYS[3]
			local disconnected_key = KEYS[4]
			local mutation_version_key = KEYS[5]

			local member = ARGV[1]
			local expected_meta = ARGV[2]
			local expected_version_exists = ARGV[3] == '1'
			local expected_version = ARGV[4]
			local expiry_ms = ARGV[5]
			local member_ttl_ms = ARGV[6]
			local key_ttl_seconds = ARGV[7]

			if redis.call('HGET', meta_key, member) ~= expected_meta then
				return 0
			end
			local mutation_version = redis.call('GET', mutation_version_key)
			if expected_version_exists then
				if mutation_version == false or mutation_version ~= expected_version then
					return 0
				end
			elseif mutation_version ~= false then
				return 0
			end

			local member_meta = redis.call('GET', member_key)
			local member_ttl = redis.call('PTTL', member_key)
			if member_meta == false or member_meta ~= expected_meta or member_ttl <= 0 then
				return 2
			end

			if mutation_version == false then
				redis.call('SET', mutation_version_key, 0, 'EX', key_ttl_seconds)
			end
			redis.call('ZADD', connected_key, expiry_ms, member)
			redis.call('PEXPIRE', member_key, member_ttl_ms)
			redis.call('EXPIRE', meta_key, key_ttl_seconds)
			redis.call('EXPIRE', connected_key, key_ttl_seconds)
			redis.call('EXPIRE', disconnected_key, key_ttl_seconds)
			redis.call('EXPIRE', mutation_version_key, key_ttl_seconds)
			return 1
			`,
			[]string{
				networkPeerMetaKey(networkId),
				networkPeerMemberKey(networkId, clientId),
				networkPeerConnectedKey(networkId),
				networkPeerDisconnectedKey(networkId),
				networkPeerMutationVersionKey(networkId, clientId),
			},
			member,
			state.metaBytes,
			hasMutationVersionInt,
			state.mutationVersion,
			expiryMs,
			ttl.Milliseconds(),
			int64(networkPeerKeyTtl/time.Second),
		).Int()
		if err != nil {
			panic(err)
		}
		refreshed = refreshedInt == 1
		conflict = refreshedInt == 0
		if refreshed {
			pruneNetworkPeers(ctx, r, networkId, pruneOperationId)
		}
	})
	return
}

// Extends the connected expiry only while the exact resident registration is
// still current. False tells the caller to load a fresh profile and re-add.
func RefreshNetworkPeer(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	residentId server.Id,
	ttl time.Duration,
) (ok bool) {
	pruneOperationId := server.NewId()
	retryNetworkPeerMutation(ctx, func() bool {
		state := getNetworkPeerMutationState(ctx, networkId, clientId)
		meta, err := loadNetworkPeerMeta(state.metaBytes)
		server.Raise(err)
		if meta == nil || meta.ResidentId != residentId {
			return true
		}
		refreshed, conflict := refreshNetworkPeerAtMutationState(
			ctx,
			networkId,
			clientId,
			ttl,
			state,
			pruneOperationId,
		)
		if refreshed {
			ok = true
			return true
		}
		if !conflict {
			return true
		}
		return false
	})
	return
}

// Removes one exact registration, advances its mutation fence, and publishes
// a disconnect marker in one transition.
func removeNetworkPeerAtMutationState(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	state networkPeerMutationState,
	operationId server.Id,
) (removed bool) {
	member := string(clientId.Bytes())
	disconnectTimeMs := server.NowUtc().UnixMilli()
	hasMutationVersionInt := 0
	if state.hasMutationVersion {
		hasMutationVersionInt = 1
	}

	server.Redis(ctx, func(r server.RedisClient) {
		removedInt, err := r.Eval(
			ctx,
			`
			local meta_key = KEYS[1]
			local member_key = KEYS[2]
			local connected_key = KEYS[3]
			local disconnected_key = KEYS[4]
			local event_id_key = KEYS[5]
			local mutation_version_key = KEYS[6]
			local mutation_receipt_key = KEYS[7]

			local member = ARGV[1]
			local expected_meta = ARGV[2]
			local expected_version_exists = ARGV[3] == '1'
			local expected_version = ARGV[4]
			local disconnect_time_ms = ARGV[5]
			local key_ttl_seconds = ARGV[6]
			local receipt_ttl_seconds = ARGV[7]

			local previous_result = redis.call('GET', mutation_receipt_key)
			if previous_result ~= false then
				return tonumber(previous_result)
			end

			if redis.call('HGET', meta_key, member) ~= expected_meta then
				return 0
			end
			local mutation_version = redis.call('GET', mutation_version_key)
			if expected_version_exists then
				if mutation_version == false or mutation_version ~= expected_version then
					return 0
				end
			elseif mutation_version ~= false then
				return 0
			end

			if mutation_version == false then
				redis.call('SET', mutation_version_key, 0, 'EX', key_ttl_seconds)
			end
			redis.call('INCR', mutation_version_key)
			redis.call('HDEL', meta_key, member)
			redis.call('DEL', member_key)
			redis.call('ZREM', connected_key, member)
			redis.call('ZADD', disconnected_key, disconnect_time_ms, member)
			redis.call('INCR', event_id_key)

			redis.call('EXPIRE', meta_key, key_ttl_seconds)
			redis.call('EXPIRE', connected_key, key_ttl_seconds)
			redis.call('EXPIRE', disconnected_key, key_ttl_seconds)
			redis.call('EXPIRE', mutation_version_key, key_ttl_seconds)
			redis.call('EXPIRE', event_id_key, key_ttl_seconds)
			redis.call('SET', mutation_receipt_key, 1, 'EX', receipt_ttl_seconds)
			return 1
			`,
			[]string{
				networkPeerMetaKey(networkId),
				networkPeerMemberKey(networkId, clientId),
				networkPeerConnectedKey(networkId),
				networkPeerDisconnectedKey(networkId),
				networkPeerEventIdKey(networkId),
				networkPeerMutationVersionKey(networkId, clientId),
				networkPeerMutationReceiptKey(networkId, operationId),
			},
			member,
			state.metaBytes,
			hasMutationVersionInt,
			state.mutationVersion,
			disconnectTimeMs,
			int64(networkPeerKeyTtl/time.Second),
			int64(networkPeerMutationReceiptTtl/time.Second),
		).Int()
		if err != nil {
			panic(err)
		}
		removed = removedInt == 1
	})
	return
}

// Removes a peer only while the exact resident registration read here is
// current, so a delayed teardown cannot delete a replacement.
func RemoveNetworkPeer(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	residentId server.Id,
) {
	operationId := server.NewId()
	retryNetworkPeerMutation(ctx, func() bool {
		state := getNetworkPeerMutationState(ctx, networkId, clientId)
		meta, err := loadNetworkPeerMeta(state.metaBytes)
		server.Raise(err)
		if meta == nil || meta.ResidentId != residentId {
			return true
		}
		if removeNetworkPeerAtMutationState(ctx, networkId, clientId, state, operationId) {
			return true
		}
		return false
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

// Updates the provide modes of a registered peer and publishes an updated
// event. An Add intent with no metadata is still fenced so its stale canonical
// read cannot commit afterward.
func UpdateNetworkPeerProvideModes(
	ctx context.Context,
	clientId server.Id,
	provideModes map[ProvideMode]bool,
) {
	networkId := GetNetworkClientNetwork(ctx, clientId)
	if networkId == nil {
		return
	}
	updateNetworkPeerProvideModes(ctx, *networkId, clientId, provideModes)
}

// Compares both values from one optimistic snapshot, then advances the fence
// before conditionally rewriting a healthy member. False means a concurrent
// mutation won and the caller must reload both values.
func updateNetworkPeerProvideModesAtMutationState(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	provideModesList []ProvideMode,
	state networkPeerMutationState,
	operationId server.Id,
) (complete bool) {
	member := string(clientId.Bytes())
	updatedMetaBytes := []byte{}
	hasUpdatedMeta := false
	if state.hasMeta {
		meta, err := loadNetworkPeerMeta(state.metaBytes)
		server.Raise(err)
		if meta != nil && meta.Peer != nil && !slices.Equal(meta.Peer.ProvideModes, provideModesList) {
			meta.Peer = cloneNetworkPeer(meta.Peer)
			meta.Peer.ProvideModes = slices.Clone(provideModesList)
			updatedMetaBytes = meta.Bytes()
			hasUpdatedMeta = true
		}
	}
	hasMetaInt := 0
	if state.hasMeta {
		hasMetaInt = 1
	}
	hasMutationVersionInt := 0
	if state.hasMutationVersion {
		hasMutationVersionInt = 1
	}
	hasUpdatedMetaInt := 0
	if hasUpdatedMeta {
		hasUpdatedMetaInt = 1
	}

	server.Redis(ctx, func(r server.RedisClient) {
		completeInt, err := r.Eval(
			ctx,
			`
			local meta_key = KEYS[1]
			local member_key = KEYS[2]
			local connected_key = KEYS[3]
			local disconnected_key = KEYS[4]
			local event_id_key = KEYS[5]
			local mutation_version_key = KEYS[6]
			local mutation_receipt_key = KEYS[7]

			local member = ARGV[1]
			local expected_meta_exists = ARGV[2] == '1'
			local expected_meta = ARGV[3]
			local expected_version_exists = ARGV[4] == '1'
			local expected_version = ARGV[5]
			local updated_meta_exists = ARGV[6] == '1'
			local updated_meta = ARGV[7]
			local key_ttl_seconds = ARGV[8]
			local receipt_ttl_seconds = ARGV[9]

			local previous_result = redis.call('GET', mutation_receipt_key)
			if previous_result ~= false then
				return tonumber(previous_result)
			end

			local current_meta = redis.call('HGET', meta_key, member)
			if expected_meta_exists then
				if current_meta == false or current_meta ~= expected_meta then
					return 0
				end
			elseif current_meta ~= false then
				return 0
			end

			local mutation_version = redis.call('GET', mutation_version_key)
			if expected_version_exists then
				if mutation_version == false or mutation_version ~= expected_version then
					return 0
				end
			elseif mutation_version ~= false then
				return 0
			end

			-- No registration and no Add intent: a later Add necessarily plants
			-- its fence after this canonical update and therefore reads it.
			if current_meta == false and mutation_version == false then
				redis.call('SET', mutation_receipt_key, 1, 'EX', receipt_ttl_seconds)
				return 1
			end

			-- Legacy registrations have metadata but no mutation key. Initialize
			-- them in this same transition before advancing the fence.
			if mutation_version == false then
				redis.call('SET', mutation_version_key, 0, 'EX', key_ttl_seconds)
			end
			redis.call('INCR', mutation_version_key)
			redis.call('EXPIRE', mutation_version_key, key_ttl_seconds)

			-- An in-flight Add has an intent key but no metadata. Advancing the
			-- key above is the entire update; its stale Add CAS must now retry.
			if current_meta == false then
				redis.call('SET', mutation_receipt_key, 1, 'EX', receipt_ttl_seconds)
				return 1
			end
			if not updated_meta_exists then
				redis.call('SET', mutation_receipt_key, 1, 'EX', receipt_ttl_seconds)
				return 1
			end

			local member_meta = redis.call('GET', member_key)
			local member_ttl_ms = redis.call('PTTL', member_key)
			if member_meta == false or member_meta ~= expected_meta or member_ttl_ms <= 0 then
				redis.call('SET', mutation_receipt_key, 1, 'EX', receipt_ttl_seconds)
				return 1
			end

			redis.call('HSET', meta_key, member, updated_meta)
			redis.call('SET', member_key, updated_meta, 'PX', member_ttl_ms)
			redis.call('INCR', event_id_key)
			redis.call('EXPIRE', meta_key, key_ttl_seconds)
			redis.call('EXPIRE', connected_key, key_ttl_seconds)
			redis.call('EXPIRE', disconnected_key, key_ttl_seconds)
			redis.call('EXPIRE', event_id_key, key_ttl_seconds)
			redis.call('SET', mutation_receipt_key, 1, 'EX', receipt_ttl_seconds)
			return 1
			`,
			[]string{
				networkPeerMetaKey(networkId),
				networkPeerMemberKey(networkId, clientId),
				networkPeerConnectedKey(networkId),
				networkPeerDisconnectedKey(networkId),
				networkPeerEventIdKey(networkId),
				networkPeerMutationVersionKey(networkId, clientId),
				networkPeerMutationReceiptKey(networkId, operationId),
			},
			member,
			hasMetaInt,
			state.metaBytes,
			hasMutationVersionInt,
			state.mutationVersion,
			hasUpdatedMetaInt,
			updatedMetaBytes,
			int64(networkPeerKeyTtl/time.Second),
			int64(networkPeerMutationReceiptTtl/time.Second),
		).Int()
		if err != nil {
			panic(err)
		}
		complete = completeInt == 1
	})
	return
}

// Retries only when the exact registry snapshot changed between its read and
// Lua transition. The shared delay, context, and attempt cap bound sustained
// churn.
func updateNetworkPeerProvideModes(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	provideModes map[ProvideMode]bool,
) bool {
	provideModesList := sortedProvideModesList(provideModes)
	operationId := server.NewId()
	return retryNetworkPeerMutation(ctx, func() bool {
		state := getNetworkPeerMutationState(ctx, networkId, clientId)
		if updateNetworkPeerProvideModesAtMutationState(
			ctx,
			networkId,
			clientId,
			provideModesList,
			state,
			operationId,
		) {
			return true
		}
		return false
	})
}

func networkPeerDisconnectMarker(clientId server.Id, disconnectTime time.Time) *NetworkPeer {
	disconnectTime = disconnectTime.UTC()
	return &NetworkPeer{
		ClientId:       clientId,
		DisconnectTime: &disconnectTime,
	}
}

// Rechecks scanned candidates against the live connected score in the same
// transition that removes them. A refresh or re-add after the scan moves the
// score past `cutoffMs`, so the stale prune cannot delete that registration.
func pruneNetworkPeerCandidates(
	ctx context.Context,
	r server.RedisClient,
	networkId server.Id,
	candidates []redis.Z,
	cutoffMs int64,
	operationId server.Id,
) (prunedCount int) {
	if len(candidates) == 0 {
		return 0
	}

	keys := []string{
		networkPeerMetaKey(networkId),
		networkPeerConnectedKey(networkId),
		networkPeerDisconnectedKey(networkId),
		networkPeerEventIdKey(networkId),
		networkPeerMutationReceiptKey(networkId, operationId),
	}
	args := []any{
		cutoffMs,
		int64(networkPeerKeyTtl / time.Second),
		int64(networkPeerMutationReceiptTtl / time.Second),
	}
	for _, candidate := range candidates {
		member := candidate.Member.(string)
		clientId := server.Id([]byte(member))
		keys = append(
			keys,
			networkPeerMemberKey(networkId, clientId),
			networkPeerMutationVersionKey(networkId, clientId),
		)
		args = append(args, member)
	}

	var err error
	prunedCount, err = r.Eval(
		ctx,
		`
		local meta_key = KEYS[1]
		local connected_key = KEYS[2]
		local disconnected_key = KEYS[3]
		local event_id_key = KEYS[4]
		local mutation_receipt_key = KEYS[5]

		local cutoff_ms = tonumber(ARGV[1])
		local key_ttl_seconds = ARGV[2]
		local receipt_ttl_seconds = ARGV[3]
		local pruned_count = 0

		local previous_result = redis.call('GET', mutation_receipt_key)
		if previous_result ~= false then
			return tonumber(previous_result)
		end

		for candidate_index = 1, #ARGV - 3 do
			local member = ARGV[candidate_index + 3]
			local member_key_index = 6 + (candidate_index - 1) * 2
			local member_key = KEYS[member_key_index]
			local mutation_version_key = KEYS[member_key_index + 1]
			local current_expiry_ms = redis.call('ZSCORE', connected_key, member)

			if current_expiry_ms ~= false and tonumber(current_expiry_ms) <= cutoff_ms then
				local mutation_version = redis.call('GET', mutation_version_key)
				if mutation_version == false then
					redis.call('SET', mutation_version_key, 0, 'EX', key_ttl_seconds)
				end
				redis.call('INCR', mutation_version_key)
				redis.call('EXPIRE', mutation_version_key, key_ttl_seconds)
				redis.call('HDEL', meta_key, member)
				redis.call('DEL', member_key)
				redis.call('ZREM', connected_key, member)
				redis.call('ZADD', disconnected_key, current_expiry_ms, member)
				pruned_count = pruned_count + 1
			end
		end

		if pruned_count > 0 then
			redis.call('INCR', event_id_key)
			redis.call('EXPIRE', meta_key, key_ttl_seconds)
			redis.call('EXPIRE', connected_key, key_ttl_seconds)
			redis.call('EXPIRE', disconnected_key, key_ttl_seconds)
			redis.call('EXPIRE', event_id_key, key_ttl_seconds)
		end
		redis.call('SET', mutation_receipt_key, pruned_count, 'EX', receipt_ttl_seconds)
		return pruned_count
		`,
		keys,
		args...,
	).Int()
	if err != nil {
		panic(err)
	}
	return
}

// Moves expired connected peers to disconnect markers and ages old markers
// out. It piggybacks on peer activity; the mutation helper makes the scan safe
// against a concurrent refresh or re-add.
func pruneNetworkPeers(
	ctx context.Context,
	r server.RedisClient,
	networkId server.Id,
	operationId server.Id,
) {
	nowMs := server.NowUtc().UnixMilli()

	expired, err := r.ZRangeByScoreWithScores(ctx, networkPeerConnectedKey(networkId), &redis.ZRangeBy{
		Min: "-inf",
		Max: strconv.FormatInt(nowMs, 10),
	}).Result()
	if err != nil {
		panic(err)
	}
	pruneNetworkPeerCandidates(ctx, r, networkId, expired, nowMs, operationId)

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

var networkPeerListenerResets = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_network_peer_listener_resets_total",
	Help: "peer listener full-read deliveries (poll mode: normal change delivery; key-event mode: registrations + resyncs + corrective repairs)",
})

func init() {
	prometheus.MustRegister(networkPeerListenerResets)
}

type networkPeerDeltaValue struct {
	peer       *NetworkPeer
	eventId    int64
	hasEventId bool
}

// NetworkPeerDelta is one immutable key-event update whose registry read is
// shared lazily by every listener for the network in one process. A listener
// worker, rather than the subscriber demux, performs the read; concurrent
// listeners wait on the same result instead of multiplying one HGET by the
// network's resident count. Callers must treat the returned peer as immutable.
type NetworkPeerDelta struct {
	clientId server.Id
	// "set" | "del" | "expired" (see NetworkPeerListener.Delta)
	event string

	loadOnce  sync.Once
	load      func() networkPeerDeltaValue
	value     networkPeerDeltaValue
	loadError any
}

// Builds a lazy delta around the supplied registry loader. Tests use the
// loader boundary to force concurrent listeners onto one deterministic read.
func newNetworkPeerDelta(
	clientId server.Id,
	event string,
	load func() networkPeerDeltaValue,
) *NetworkPeerDelta {
	return &NetworkPeerDelta{
		clientId: clientId,
		event:    event,
		load:     load,
	}
}

// NewNetworkPeerDelta prepares a lazy peer metadata + registry-version read
// for process-wide listener fanout. Constructing it does no Redis work, so the
// key-event subscriber remains a nonblocking demultiplexer. The first listener
// to consume it executes one pipelined read; all other listeners share that
// exact result.
func NewNetworkPeerDelta(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	event string,
) *NetworkPeerDelta {
	return newNetworkPeerDelta(clientId, event, func() networkPeerDeltaValue {
		value := networkPeerDeltaValue{hasEventId: true}
		server.Redis(ctx, func(r server.RedisClient) {
			pipe := r.Pipeline()
			var peerCmd *redis.StringCmd
			if event == "set" {
				peerCmd = pipe.HGet(ctx, networkPeerMetaKey(networkId), string(clientId.Bytes()))
			}
			eventIdCmd := pipe.Get(ctx, networkPeerEventIdKey(networkId))
			_, err := pipe.Exec(ctx)
			if err != nil && err != server.RedisNil {
				panic(err)
			}

			if peerCmd != nil {
				metaBytes, err := peerCmd.Bytes()
				if err != nil && err != server.RedisNil {
					panic(err)
				}
				if err == nil {
					meta, _ := loadNetworkPeerMeta(metaBytes)
					if meta != nil {
						value.peer = meta.Peer
					}
				}
			}

			eventId, err := eventIdCmd.Int64()
			if err != nil && err != server.RedisNil {
				panic(err)
			}
			value.eventId = eventId
		})
		return value
	})
}

// Resolves the shared registry value once and contains a loader failure so
// every listener can independently request its corrective resync.
func (self *NetworkPeerDelta) loadValue() (value networkPeerDeltaValue, ok bool) {
	self.loadOnce.Do(func() {
		self.loadError = server.HandleError(func() {
			self.value = self.load()
		})
	})
	return self.value, self.loadError == nil
}

type networkPeerStateHash [sha256.Size]byte

// NetworkPeerSnapshot is an immutable, prepared full-state reconcile shared
// by every listener for one network in a process. The per-peer and aggregate
// hashes are computed once, rather than JSON-encoding every peer again in
// every resident listener.
//
// Its fields are deliberately private: callers may distribute a snapshot but
// cannot mutate the state used for convergence decisions.
type NetworkPeerSnapshot struct {
	eventId   int64
	peers     []*NetworkPeer
	peerState map[server.Id]networkPeerStateHash
	stateHash networkPeerStateHash
}

func networkPeerStateEntryHash(peer *NetworkPeer) networkPeerStateHash {
	kind := byte(1)
	var encoded []byte
	if peer.DisconnectTime != nil {
		// The exact disconnect time can differ between a key-event marker and
		// the authoritative zset score. Both represent the same state.
		kind = 0
	} else {
		var err error
		encoded, err = json.Marshal(peer)
		if err != nil {
			panic(err)
		}
	}

	input := make([]byte, 0, 1+len(peer.ClientId.Bytes())+len(encoded))
	input = append(input, kind)
	input = append(input, peer.ClientId.Bytes()...)
	input = append(input, encoded...)
	return sha256.Sum256(input)
}

func xorNetworkPeerStateHash(dst *networkPeerStateHash, value networkPeerStateHash) {
	for i := range dst {
		dst[i] ^= value[i]
	}
}

// PrepareNetworkPeerSnapshot prepares a GetNetworkPeers result once for
// process-wide fanout. The caller and listeners must treat peers as immutable.
func PrepareNetworkPeerSnapshot(eventId int64, peers []*NetworkPeer) *NetworkPeerSnapshot {
	snapshot := &NetworkPeerSnapshot{
		eventId:   eventId,
		peers:     peers,
		peerState: make(map[server.Id]networkPeerStateHash, len(peers)),
	}
	for _, peer := range peers {
		hash := networkPeerStateEntryHash(peer)
		if oldHash, ok := snapshot.peerState[peer.ClientId]; ok {
			xorNetworkPeerStateHash(&snapshot.stateHash, oldHash)
		}
		snapshot.peerState[peer.ClientId] = hash
		xorNetworkPeerStateHash(&snapshot.stateHash, hash)
	}
	return snapshot
}

type NetworkPeerListener struct {
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	networkId     server.Id
	callback      func(*NetworkPeerEvent)
	pollInterval  time.Duration
	fullReadEvery int

	// key-event inputs (PEERSSTREAMS2.md). `deltas` carries per-peer key
	// events; `resync` forces the next tick to full-read. Both are fed
	// non-blocking by the exchange's key-event subscriber; a full delta
	// buffer degrades to a resync, never to a lost change.
	deltas      chan *NetworkPeerDelta
	resync      chan struct{}
	forceResync atomic.Bool

	snapshotLock    sync.Mutex
	pendingSnapshot *NetworkPeerSnapshot
	snapshotReady   chan struct{}

	// Nil outside package tests. The callback runs after the listener worker
	// has joined and immediately before CloseAndWait returns.
	afterCloseWaitForTest func()
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
		done:          make(chan struct{}),
		networkId:     networkId,
		callback:      callback,
		pollInterval:  pollInterval,
		fullReadEvery: fullReadEvery,
		deltas:        make(chan *NetworkPeerDelta, 32),
		resync:        make(chan struct{}, 1),
		snapshotReady: make(chan struct{}, 1),
	}
	go func() {
		defer close(npl.done)
		server.HandleError(npl.run)
	}()
	return npl
}

// Delta feeds one per-peer key event ("set" = registered/changed, "del" or
// "expired" = disconnected). Non-blocking: when the buffer is full the
// listener degrades to a resync — a delayed full read, never a lost change.
// Safe to call from the subscriber's demux goroutine.
func (self *NetworkPeerListener) Delta(clientId server.Id, event string) {
	self.ApplyDelta(newNetworkPeerDelta(clientId, event, func() networkPeerDeltaValue {
		value := networkPeerDeltaValue{}
		if event == "set" {
			value.peer = GetNetworkPeerMember(self.ctx, self.networkId, clientId)
		}
		return value
	}))
}

// DeltaWithEventId is Delta with the registry version observed once by the
// process-wide subscriber. Advancing the local version after a delivered
// delta prevents every listener from redundantly full-reading the same
// network on its next corrective tick.
func (self *NetworkPeerListener) DeltaWithEventId(clientId server.Id, event string, eventId int64) {
	self.ApplyDelta(newNetworkPeerDelta(clientId, event, func() networkPeerDeltaValue {
		value := networkPeerDeltaValue{
			eventId:    eventId,
			hasEventId: true,
		}
		if event == "set" {
			value.peer = GetNetworkPeerMember(self.ctx, self.networkId, clientId)
		}
		return value
	}))
}

// ApplyDelta non-blockingly supplies a prepared delta. Multiple listeners may
// receive the same delta; its lazy registry read executes once across them. A
// full input buffer degrades to a resync, never a lost change.
func (self *NetworkPeerListener) ApplyDelta(delta *NetworkPeerDelta) {
	select {
	case self.deltas <- delta:
	default:
		self.Resync()
	}
}

// ApplySnapshot supplies a prepared snapshot fetched once for all listeners
// of a network by the process-wide key-event subscriber. Only the newest
// pending snapshot is retained; snapshots are full state, so replacing an
// older one is lossless.
func (self *NetworkPeerListener) ApplySnapshot(snapshot *NetworkPeerSnapshot) {
	self.snapshotLock.Lock()
	self.pendingSnapshot = snapshot
	self.snapshotLock.Unlock()
	select {
	case self.snapshotReady <- struct{}{}:
	default:
	}
}

// Resync forces the next wake-up to full-read regardless of the version
// counter (events may have been dropped). Non-blocking, coalescing.
func (self *NetworkPeerListener) Resync() {
	self.forceResync.Store(true)
	select {
	case self.resync <- struct{}{}:
	default:
	}
}

func (self *NetworkPeerListener) run() {
	defer self.cancel()

	// the last synced version. `!=` (never `<`): the counter moves backward
	// when the registry is flushed, ttl-expires, or is rebuilt — any mismatch
	// resyncs from a full read.
	var eventId int64
	synced := false
	peerState := map[server.Id]networkPeerStateHash{}
	var stateHash networkPeerStateHash

	applyReset := func(snapshot *NetworkPeerSnapshot) {
		stateChanged := len(peerState) != len(snapshot.peerState) || stateHash != snapshot.stateHash
		// Always acknowledge the authoritative version, even when a delivered
		// delta already brought local state to the same result.
		eventId = snapshot.eventId
		if !synced || stateChanged {
			peerState = make(map[server.Id]networkPeerStateHash, len(snapshot.peerState))
			for clientId, hash := range snapshot.peerState {
				peerState[clientId] = hash
			}
			stateHash = snapshot.stateHash
			// in key-event mode (long corrective poll) this counts corrective
			// deliveries — expected near zero beyond registrations/resyncs; a
			// sustained rate means events are being dropped (PEERSSTREAMS2.md)
			networkPeerListenerResets.Inc()
			synced = true

			resetEvent := &NetworkPeerEvent{
				NetworkPeerEventType: NetworkPeerEventTypeReset,
				EventId:              snapshot.eventId,
				Peers:                snapshot.peers,
			}
			self.callback(resetEvent)
		}
	}
	reset := func() {
		resetEventId, resetPeers := GetNetworkPeers(self.ctx, self.networkId)
		applyReset(PrepareNetworkPeerSnapshot(resetEventId, resetPeers))
	}

	// handleDelta applies one per-peer key event (PEERSSTREAMS2.md): `set`
	// reads the single member and delivers an Updated event; `del`/`expired`
	// delivers a disconnect marker. A subscriber-supplied delta advances the
	// shared observed version; legacy direct deltas leave reconciliation to the
	// corrective poll.
	handleDelta := func(delta *NetworkPeerDelta) {
		value, ok := delta.loadValue()
		if !ok {
			self.Resync()
			return
		}
		if synced && value.hasEventId {
			eventId = value.eventId
		}
		switch delta.event {
		case "set":
			if peer := value.peer; peer != nil {
				if oldHash, ok := peerState[delta.clientId]; ok {
					xorNetworkPeerStateHash(&stateHash, oldHash)
				}
				newHash := networkPeerStateEntryHash(peer)
				peerState[delta.clientId] = newHash
				xorNetworkPeerStateHash(&stateHash, newHash)
				self.callback(&NetworkPeerEvent{
					NetworkPeerEventType: NetworkPeerEventTypeUpdated,
					EventId:              eventId,
					Peers:                []*NetworkPeer{peer},
				})
				return
			}
			// the registration raced a remove; fall through to the marker
			fallthrough
		case "del", "expired":
			if oldHash, ok := peerState[delta.clientId]; ok {
				xorNetworkPeerStateHash(&stateHash, oldHash)
			}
			marker := networkPeerDisconnectMarker(delta.clientId, server.NowUtc())
			newHash := networkPeerStateEntryHash(marker)
			peerState[delta.clientId] = newHash
			xorNetworkPeerStateHash(&stateHash, newHash)
			self.callback(&NetworkPeerEvent{
				NetworkPeerEventType: NetworkPeerEventTypeRemoved,
				EventId:              eventId,
				Peers:                []*NetworkPeer{marker},
			})
		}
	}

	jitterInterval := func(consecutiveErrors int) time.Duration {
		// ±20% jitter spreads fleet polls; error backoff (up to 4x) keeps a
		// sick slot from being hammered
		interval := self.pollInterval + time.Duration((mathrand.Float64()*0.4-0.2)*float64(self.pollInterval))
		return interval << consecutiveErrors
	}

	consecutiveErrors := 0
	tick := 0
	// the poll deadline is absolute so a stream of deltas cannot starve the
	// corrective poll (PEERSSTREAMS2.md §5.4)
	nextPollTime := time.Now().Add(jitterInterval(0))
	for {
		select {
		case <-self.ctx.Done():
			return
		case delta := <-self.deltas:
			// contain redis panics to the delta; a failed delta read degrades
			// to a forced resync, never a lost change. Resync (not a bare
			// flag store) so the repair wakes promptly instead of waiting
			// for the next poll
			if r := server.HandleError(func() {
				handleDelta(delta)
			}); r != nil {
				self.Resync()
			}
			continue
		case <-self.snapshotReady:
			self.snapshotLock.Lock()
			snapshot := self.pendingSnapshot
			self.pendingSnapshot = nil
			self.snapshotLock.Unlock()
			if snapshot != nil {
				if r := server.HandleError(func() {
					applyReset(snapshot)
				}); r != nil {
					self.Resync()
				}
			}
			continue
		case <-self.resync:
		case <-time.After(time.Until(nextPollTime)):
		}

		tick += 1
		force := self.forceResync.Swap(false)
		// contain redis panics to the tick: a failed poll must never kill the
		// listener (2026-07-15: panicked listeners died permanently and their
		// clients silently stopped receiving peer updates). The periodic full
		// read (fullReadEvery) re-reads the snapshot as a cheap hygiene check;
		// it still only delivers on a version change. A forced resync (dropped
		// or failed key events, subscriber (re)connect) always full-reads.
		if r := server.HandleError(func() {
			if force {
				synced = false
			}
			if !synced ||
				(0 < self.fullReadEvery && tick%self.fullReadEvery == 0) ||
				GetNetworkPeerEventId(self.ctx, self.networkId) != eventId {
				reset()
			}
		}); r != nil {
			consecutiveErrors = min(consecutiveErrors+1, 2)
			if force {
				// the resync did not complete; keep it pending
				self.forceResync.Store(true)
			}
		} else {
			consecutiveErrors = 0
		}
		nextPollTime = time.Now().Add(jitterInterval(consecutiveErrors))
	}
}

func (self *NetworkPeerListener) Close() {
	self.cancel()
}

// CloseAndWait cancels the listener and joins its poll worker and any callback
// already admitted by that worker. A listener callback must not call this
// method because it would wait for itself; it may call the non-joining Close.
func (self *NetworkPeerListener) CloseAndWait() {
	self.Close()
	<-self.done
	if self.afterCloseWaitForTest != nil {
		self.afterCloseWaitForTest()
	}
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
