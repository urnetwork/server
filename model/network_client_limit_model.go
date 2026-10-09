package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/server"
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

// networkTopLevelClientLimitInTx reads the limit inside the provisioning
// transaction that enforces it.
func networkTopLevelClientLimitInTx(ctx context.Context, tx server.PgTx, networkId server.Id) int {
	limit := LimitTopLevelClientIdsPerNetwork
	result, err := tx.Query(
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
		}
	})
	return limit
}

func GetNetworkTopLevelClientLimit(ctx context.Context, networkId server.Id) *NetworkTopLevelClientLimit {
	limit := &NetworkTopLevelClientLimit{
		NetworkId: networkId,
		Limit:     LimitTopLevelClientIdsPerNetwork,
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
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
				server.Raise(result.Scan(&limit.Limit))
				limit.Override = true
			}
		})
	})
	return limit
}

// SetNetworkTopLevelClientLimit sets the Embed plan limit for a network.
func SetNetworkTopLevelClientLimit(ctx context.Context, networkId server.Id, limit int) error {
	if limit < 1 || MaxNetworkTopLevelClientLimit < limit {
		return fmt.Errorf("The limit must be between 1 and %d.", MaxNetworkTopLevelClientLimit)
	}
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = nil
		exists := false
		result, err := tx.Query(ctx, `SELECT true FROM network WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			exists = result.Next()
		})
		if !exists {
			returnErr = ErrNetworkNotFound
			return
		}
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
			server.NowUtc(),
		))
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
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM network_top_level_client_limit WHERE network_id = $1`,
			networkId,
		))
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
	if limit, override := networkClientLimitOverride(ctx, networkId); override {
		return limit
	}
	return Pro().MaxConcurrentClients(IsProNetwork(ctx, networkId))
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
