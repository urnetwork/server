package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// The Embed plan's per-network client allowance (EMBED1.md).
//
// A network without a `network_top_level_client_limit` row uses the defaults:
// `LimitTopLevelClientIdsPerNetwork` for the AuthNetworkClient create cap and
// its tier's pro.yml `concurrent_clients` for the concurrent connection limit.
// Ops sets a row for a network on an Embed plan (`bringyourctl network
// client-limit`), and the row's limit is then the network's client allowance
// for both: the top-level client limit and the concurrent connection limit
// (NetworkConcurrentClientsExceeded, CanConnectNetworkPeer and the provider
// intent's normal client limit). Both stay gated by enforce_concurrent_clients.
// The peer valve (peer_model.go) keeps the constant, so a large embedded
// network keeps its peer list off and its users stay invisible to each other.

// The largest limit ops may set. The create cap counts with `LIMIT limit+1`,
// so the bound also bounds that scan.
const MaxNetworkTopLevelClientLimit = 10_000_000

var ErrNetworkNotFound = errors.New("Network does not exist.")

type NetworkTopLevelClientLimit struct {
	NetworkId server.Id
	Limit     int
	// false when the network uses the default
	Override bool
}

// readNetworkTopLevelClientLimit returns the network's top-level client limit
// and whether it is an Embed plan override rather than the default.
func readNetworkTopLevelClientLimit(ctx context.Context, query server.PgCanQuery, networkId server.Id) (limit int, override bool) {
	limit = LimitTopLevelClientIdsPerNetwork
	result, err := query.Query(
		ctx,
		`
			SELECT top_level_client_limit
			FROM network_top_level_client_limit
			WHERE network_id = $1
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&limit))
			override = true
		}
	})
	return
}

// networkTopLevelClientLimitInTx reads the limit inside the provisioning
// transaction that enforces it.
func networkTopLevelClientLimitInTx(ctx context.Context, tx server.PgTx, networkId server.Id) int {
	limit, _ := readNetworkTopLevelClientLimit(ctx, tx, networkId)
	return limit
}

// The most active top-level clients the create cap's count reads to find the
// network's devices among them (countNetworkActiveTopLevelClients). A network
// whose top-level clients are mostly hosted proxies can hold thousands more of
// them than devices, and the count skips those, so the read is bounded rather
// than walking them all on every create. A network with more top-level clients
// than this is counted from the first this many, which can only undercount: the
// cap then lets a create through rather than refusing one. An Embed plan row
// above it reads up to its limit. Variable only so tests can change it.
var topLevelClientCountExamineBudget = 50_000

// Testing_SetTopLevelClientCountExamineBudget sets the read bound of the create
// cap's count and returns a function that restores it. Tests that use it must
// not run in parallel.
func Testing_SetTopLevelClientCountExamineBudget(budget int) func() {
	prev := topLevelClientCountExamineBudget
	topLevelClientCountExamineBudget = budget
	return func() {
		topLevelClientCountExamineBudget = prev
	}
}

// countNetworkActiveTopLevelClients counts what the AuthNetworkClient create cap
// counts: the network's devices, its active top-level clients (no
// source_client_id) that are neither provider installs nor hosted proxies. A
// hosted proxy is governed by its own cap and create rate
// (network_client_proxy_limit.go), not by this one (owner decision 2026-11-08),
// so a network with many proxies can still add its devices. The count stops at
// scanLimit devices: the create cap passes its limit + 1, since only the
// threshold matters there, and GET /network/embed passes the largest allowance
// + 1, so its count is exact for any allowance. It reads at most examineLimit
// top-level clients to find them (topLevelClientCountExamineBudget).
func countNetworkActiveTopLevelClients(ctx context.Context, query server.PgCanQuery, networkId server.Id, scanLimit int, examineLimit int) int {
	count := 0
	result, err := query.Query(
		ctx,
		`
			SELECT COUNT(*) AS top_level_client_count
			FROM (
				SELECT 1
				FROM (
					SELECT network_client.client_id
					FROM network_client
					WHERE
						network_client.network_id = $1 AND
						network_client.active = true AND
						network_client.source_client_id IS NULL
					LIMIT $3
				) top_level
				WHERE
					NOT EXISTS (
						SELECT 1 FROM network_client_provider_intent
						WHERE network_client_provider_intent.client_id = top_level.client_id
					) AND
					NOT EXISTS (
						SELECT 1 FROM proxy_device_config
						WHERE proxy_device_config.client_id = top_level.client_id
					)
				LIMIT $2
			) t
		`,
		networkId,
		scanLimit,
		max(scanLimit, examineLimit),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&count))
		}
	})
	return count
}

func GetNetworkTopLevelClientLimit(ctx context.Context, networkId server.Id) *NetworkTopLevelClientLimit {
	limit := &NetworkTopLevelClientLimit{
		NetworkId: networkId,
	}
	server.Db(ctx, func(conn server.PgConn) {
		limit.Limit, limit.Override = readNetworkTopLevelClientLimit(ctx, conn, networkId)
	})
	return limit
}

func networkTopLevelClientLimitMessage(limit int) string {
	if limit < 1 || MaxNetworkTopLevelClientLimit < limit {
		return fmt.Sprintf("The limit must be between 1 and %d.", MaxNetworkTopLevelClientLimit)
	}
	return ""
}

// networkExistsInTx reports whether the network exists.
func networkExistsInTx(ctx context.Context, tx server.PgTx, networkId server.Id) (exists bool) {
	result, err := tx.Query(ctx, `SELECT true FROM network WHERE network_id = $1`, networkId)
	server.WithPgResult(result, err, func() {
		exists = result.Next()
	})
	return
}

func upsertNetworkTopLevelClientLimitInTx(ctx context.Context, tx server.PgTx, networkId server.Id, limit int, now time.Time) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_top_level_client_limit (
				network_id,
				top_level_client_limit,
				update_time
			)
			VALUES ($1, $2, $3)
			ON CONFLICT (network_id) DO UPDATE
			SET
				top_level_client_limit = $2,
				update_time = $3
		`,
		networkId,
		limit,
		now,
	))
}

func deleteNetworkTopLevelClientLimitInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`DELETE FROM network_top_level_client_limit WHERE network_id = $1`,
		networkId,
	))
}

// SetNetworkTopLevelClientLimit sets the Embed plan limit for a network.
func SetNetworkTopLevelClientLimit(ctx context.Context, networkId server.Id, limit int) error {
	if message := networkTopLevelClientLimitMessage(limit); message != "" {
		return errors.New(message)
	}
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = nil
		if !networkExistsInTx(ctx, tx, networkId) {
			returnErr = ErrNetworkNotFound
			return
		}
		upsertNetworkTopLevelClientLimitInTx(ctx, tx, networkId, limit, server.NowUtc())
	})
	if returnErr == nil {
		// this process enforces the change at once; others within the cache ttl
		networkClientLimitLocal.Remove(networkId)
	}
	return returnErr
}

// ClearNetworkTopLevelClientLimit returns the network to the default limit.
func ClearNetworkTopLevelClientLimit(ctx context.Context, networkId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		deleteNetworkTopLevelClientLimitInTx(ctx, tx, networkId)
	})
	networkClientLimitLocal.Remove(networkId)
}

// The concurrent gates read the override on every connection activation while
// enforcement is on, and ops changes it rarely, so each process keeps it for a
// short ttl. Set and Clear refresh this process at once; other processes pick
// the change up within the ttl.
const networkClientLimitLocalCacheTtl = 30 * time.Second

// A full cache restarts rather than growing; misses reload from the db.
const networkClientLimitLocalCacheMaxSize = 8192

type networkClientLimitLocalEntry struct {
	limit    int
	override bool
	expiry   time.Time
}

type networkClientLimitLocalCache struct {
	stateLock sync.Mutex
	entries   map[server.Id]networkClientLimitLocalEntry
}

func newNetworkClientLimitLocalCache() *networkClientLimitLocalCache {
	return &networkClientLimitLocalCache{
		entries: map[server.Id]networkClientLimitLocalEntry{},
	}
}

func (self *networkClientLimitLocalCache) Get(networkId server.Id, now time.Time) (entry networkClientLimitLocalEntry, ok bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry, ok = self.entries[networkId]
	if ok && !now.Before(entry.expiry) {
		delete(self.entries, networkId)
		return networkClientLimitLocalEntry{}, false
	}
	return entry, ok
}

func (self *networkClientLimitLocalCache) Put(networkId server.Id, entry networkClientLimitLocalEntry) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.entries[networkId]; !ok && networkClientLimitLocalCacheMaxSize <= len(self.entries) {
		clear(self.entries)
	}
	self.entries[networkId] = entry
}

func (self *networkClientLimitLocalCache) Remove(networkId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.entries, networkId)
}

func (self *networkClientLimitLocalCache) Clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	clear(self.entries)
}

var networkClientLimitLocal = newNetworkClientLimitLocalCache()

// Testing_ClearNetworkClientLimitCache drops this process's cached overrides,
// so a test observes a row written directly to the db.
func Testing_ClearNetworkClientLimitCache() {
	networkClientLimitLocal.Clear()
}

// networkClientLimitOverride returns the network's Embed plan limit and
// whether it has one, through the process cache.
func networkClientLimitOverride(ctx context.Context, networkId server.Id) (limit int, override bool) {
	now := server.NowUtc()
	if entry, ok := networkClientLimitLocal.Get(networkId, now); ok {
		return entry.limit, entry.override
	}
	clientLimit := GetNetworkTopLevelClientLimit(ctx, networkId)
	networkClientLimitLocal.Put(networkId, networkClientLimitLocalEntry{
		limit:    clientLimit.Limit,
		override: clientLimit.Override,
		expiry:   now.Add(networkClientLimitLocalCacheTtl),
	})
	return clientLimit.Limit, clientLimit.Override
}

// networkConcurrentClientLimit is the network's concurrent connected top-level
// client limit, whether or not it is enforced: the Embed plan row's limit when
// the network has one, otherwise its tier's pro.yml concurrent_clients. Zero or
// less is unlimited. The tier is only read for a network without a row.
func networkConcurrentClientLimit(ctx context.Context, networkId server.Id) int {
	limit, _, _ := networkConcurrentClientLimitTier(ctx, networkId)
	return limit
}

// networkConcurrentClientLimitTier is networkConcurrentClientLimit with where
// the limit comes from: override is true for an Embed plan row, and isPro is
// the network's tier, read live (never from a jwt claim) and only for a network
// without a row, so it is false whenever override is true.
func networkConcurrentClientLimitTier(ctx context.Context, networkId server.Id) (limit int, override bool, isPro bool) {
	if limit, override := networkClientLimitOverride(ctx, networkId); override {
		return limit, true, false
	}
	isPro = IsProNetwork(ctx, networkId)
	return Pro().MaxConcurrentClients(isPro), false, isPro
}

// networkConcurrentClientLimitRefusal is the client-creation refusal of a
// network that already has its plan's full complement of connected top-level
// clients, or nil when there is room. While enforcement is dark it is nil at
// once, with no redis and no db lookup. See concurrentClientLimitRefusal for
// what the refusal offers.
func networkConcurrentClientLimitRefusal(ctx context.Context, networkId server.Id) *AuthNetworkClientError {
	// dark, or no pro.yml at all -> no limit, and no i/o to find that out
	if !Pro().EnforceConcurrentClients {
		return nil
	}
	limit, override, isPro := networkConcurrentClientLimitTier(ctx, networkId)
	connectedCount := GetNetworkEnforceableConnectedCount(ctx, networkId)
	if !concurrentClientLimitExceeded(limit, connectedCount) {
		return nil
	}
	return concurrentClientLimitRefusal(override, isPro, Pro().MaxConcurrentClients(true), connectedCount)
}

// ClientLimitUpgradeClaim is the client_limit_upgrade claim of a client token
// of the network (session.ByJwt.ClientLimitUpgrade): whether UR Pro raises the
// network's concurrent client limit. The apps' sdk offers the upgrade for a
// client limit hold only when it is true, since the hold's close says nothing
// about the plan. Pro is the network's live tier, read by the caller; the
// Embed allowance comes through the process cache.
func ClientLimitUpgradeClaim(ctx context.Context, networkId server.Id, isPro bool) *bool {
	_, override := networkClientLimitOverride(ctx, networkId)
	return clientLimitUpgradeClaim(override, isPro)
}

// clientLimitUpgradeClaim is ClientLimitUpgradeClaim from what decides it: no
// plan lifts an Embed allowance, a Pro network is already on the highest
// plan, and a free network gains only when Pro's limit is higher than free's
// (unlimited counts as higher).
func clientLimitUpgradeClaim(override bool, isPro bool) *bool {
	freeLimit := Pro().MaxConcurrentClients(false)
	proLimit := Pro().MaxConcurrentClients(true)
	raises := !override && !isPro && 0 < freeLimit && (proLimit <= 0 || freeLimit < proLimit)
	return &raises
}

// concurrentLimitCreationExempt reports whether a create skips the concurrent
// client limit's creation gate: the sign-in of a URnetwork app (owner decision
// 2026-11-08). The app's client is created, and while the network is over its
// limit the connection gate (CanConnectNetworkPeer) holds the device off, where
// the app shows what to do: Get Pro, and its device list to free a slot. A
// creation refused at sign-in left the new device with no session to reach
// either.
//
// An app is a caller whose client info names one of the apps' device types,
// creating a device rather than a hosted proxy (a proxy keeps the gate, which
// ur.io and the extension prompt on), with a network token: never an API key,
// whose caller is an agent. The client info is the caller's own claim. A caller
// that claims an app only gets clients that the connection gate still holds off
// past the limit, so it uses no more than the limit, and the top-level client
// cap still bounds how many it creates. Devices have no create rate (only
// hosted proxies do): such a caller can create devices up to the cap, remove
// them and create again, as any network under its limit already could.
func concurrentLimitCreationExempt(authClient *AuthNetworkClientArgs, clientSession *session.ClientSession) bool {
	if authClient.ProxyConfig != nil || clientSession.ApiKeyAuthenticated {
		return false
	}
	switch clientSession.ClientInfo.DeviceType {
	case "android", "ios", "macos", "windows", "linux":
		return true
	default:
		return false
	}
}

// concurrentClientLimitRefusal is the refusal of a network over its concurrent
// connected client limit. It asks for an upgrade only when an upgrade lifts the
// limit: a network on the free tier, with no Embed plan row, whose Pro tier
// allows more than `connectedCount` (a `proLimit` of zero or less is
// unlimited). A Pro network at the Pro tier's limit, and a network whose
// allowance is an Embed plan row that no plan changes, are told to free a slot
// instead. The apps show the upgrade flow only for upgrade_required, and the
// auth-client handler quotes an x402 Pro month only for it, so neither sells a
// Pro plan that would not let the client connect.
func concurrentClientLimitRefusal(override bool, isPro bool, proLimit int, connectedCount int) *AuthNetworkClientError {
	if !override && !isPro && (proLimit <= 0 || connectedCount < proLimit) {
		return &AuthNetworkClientError{
			ClientLimitExceeded: true,
			UpgradeRequired:     true,
			Message:             "Your plan's concurrent client limit is reached. Upgrade to UR Pro to connect more clients.",
		}
	}
	return &AuthNetworkClientError{
		ClientLimitExceeded: true,
		Message:             "Your plan's concurrent client limit is reached. Remove or disconnect another client to connect this one.",
	}
}

// concurrentClientLimitExceeded reports whether a network with `connectedCount`
// connected top-level clients has no room for one more under `limit`. Like
// ProConfig.ConcurrentClientsExceeded it folds in the enforce_concurrent_clients
// rollout switch, so while enforcement is dark it is always false.
func concurrentClientLimitExceeded(limit int, connectedCount int) bool {
	if !Pro().EnforceConcurrentClients {
		return false
	}
	if limit <= 0 {
		return false
	}
	return limit <= connectedCount
}
