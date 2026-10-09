package model

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Per-network Embed enablement (EMBED1.md).
//
// Embed plans are sold with vetting and a contract, and the team then enables
// Embed for the customer's network (`bringyourctl network embed --enable`). A
// network_embed row marks the network as Embed-enabled. Every other network is
// refused by the Embed APIs — the data-cap routes and the ACL-group routes —
// with NetworkEmbedNotEnabledMessage, before any other work.
//
// Only the APIs are gated. Caps and ACL groups already stored keep being
// enforced after a disable: escrow admission and peer isolation read their own
// tables and never look at this flag.

const NetworkEmbedNotEnabledMessage = "Embed isn't enabled for this network."

const networkEmbedSessionMessage = "Requires the network's root token or an API key."

// readNetworkEmbedEnabled reports whether the network has a network_embed row.
func readNetworkEmbedEnabled(ctx context.Context, query server.PgCanQuery, networkId server.Id) (enabled bool) {
	result, err := query.Query(
		ctx,
		`SELECT true FROM network_embed WHERE network_id = $1`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		enabled = result.Next()
	})
	return
}

// EnableNetworkEmbed enables Embed for a network. With a client limit it also
// sets the network's client allowance (network_client_limit_model.go) in the
// same transaction. Enabling an enabled network keeps its first enable time.
func EnableNetworkEmbed(ctx context.Context, networkId server.Id, clientLimit *int) error {
	if clientLimit != nil {
		if message := networkTopLevelClientLimitMessage(*clientLimit); message != "" {
			return errors.New(message)
		}
	}
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = nil
		if !networkExistsInTx(ctx, tx, networkId) {
			returnErr = ErrNetworkNotFound
			return
		}
		now := server.NowUtc()
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_embed (
					network_id,
					enable_time
				)
				VALUES ($1, $2)
				ON CONFLICT (network_id) DO NOTHING
			`,
			networkId,
			now,
		))
		if clientLimit != nil {
			upsertNetworkTopLevelClientLimitInTx(ctx, tx, networkId, *clientLimit, now)
		}
	})
	if returnErr == nil {
		// this process sees the change at once; others within the cache ttls
		networkEmbedLocal.Remove(networkId)
		networkClientLimitLocal.Remove(networkId)
	}
	return returnErr
}

// DisableNetworkEmbed disables Embed for a network and returns it to the
// default client limits. Its stored caps and ACL groups stay enforced.
func DisableNetworkEmbed(ctx context.Context, networkId server.Id) error {
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = nil
		if !networkExistsInTx(ctx, tx, networkId) {
			returnErr = ErrNetworkNotFound
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM network_embed WHERE network_id = $1`,
			networkId,
		))
		deleteNetworkTopLevelClientLimitInTx(ctx, tx, networkId)
	})
	if returnErr == nil {
		networkEmbedLocal.Remove(networkId)
		networkClientLimitLocal.Remove(networkId)
	}
	return returnErr
}

type NetworkEmbed struct {
	Enabled bool `json:"enabled"`
	// the effective top-level client allowance: the Embed plan override, or the
	// default
	ClientLimit int `json:"client_limit"`
	// exactly what the create cap counts (countNetworkActiveTopLevelClients)
	ActiveClientCount int `json:"active_client_count"`
}

type NetworkEmbedError struct {
	Message string `json:"message"`
}

// On success the embed fields; on a refusal only `error`.
type NetworkEmbedResult struct {
	*NetworkEmbed
	Error *NetworkEmbedError `json:"error,omitempty"`
}

// GetNetworkEmbed reads a network's Embed state from the db (not the cache).
func GetNetworkEmbed(ctx context.Context, networkId server.Id) *NetworkEmbed {
	embed := &NetworkEmbed{}
	server.Db(ctx, func(conn server.PgConn) {
		embed.Enabled = readNetworkEmbedEnabled(ctx, conn, networkId)
		embed.ClientLimit, _ = readNetworkTopLevelClientLimit(ctx, conn, networkId)
		embed.ActiveClientCount = countNetworkActiveTopLevelClients(ctx, conn, networkId, MaxNetworkTopLevelClientLimit+1)
	})
	return embed
}

// GetNetworkEmbedStatus reads the caller's network's Embed state
// (GET /network/embed). Only a network session — the root JWT or an API key —
// may read it. It is not gated: a network without Embed reads enabled=false.
func GetNetworkEmbedStatus(clientSession *session.ClientSession) (*NetworkEmbedResult, error) {
	if !clientDataCapNetworkSession(clientSession) {
		return &NetworkEmbedResult{Error: &NetworkEmbedError{Message: networkEmbedSessionMessage}}, nil
	}
	return &NetworkEmbedResult{
		NetworkEmbed: GetNetworkEmbed(clientSession.Ctx, clientSession.ByJwt.NetworkId),
	}, nil
}

// networkEmbedRefused reports whether the session's network may not use the
// Embed APIs. The gated routes check it first. A session without a network is
// left to the route's own session checks.
func networkEmbedRefused(clientSession *session.ClientSession) bool {
	if clientSession == nil || clientSession.ByJwt == nil || clientSession.ByJwt.NetworkId == (server.Id{}) {
		return false
	}
	return !NetworkEmbedEnabled(clientSession.Ctx, clientSession.ByJwt.NetworkId)
}

// Every Embed API call checks the flag, and ops changes it rarely, so each
// process keeps it for a short ttl. Enable and Disable refresh this process at
// once; other processes pick the change up within the ttl.
const networkEmbedLocalCacheTtl = 30 * time.Second

// A full cache restarts rather than growing; misses reload from the db.
const networkEmbedLocalCacheMaxSize = 8192

type networkEmbedLocalEntry struct {
	enabled bool
	expiry  time.Time
}

type networkEmbedLocalCache struct {
	stateLock sync.Mutex
	entries   map[server.Id]networkEmbedLocalEntry
}

func newNetworkEmbedLocalCache() *networkEmbedLocalCache {
	return &networkEmbedLocalCache{
		entries: map[server.Id]networkEmbedLocalEntry{},
	}
}

func (self *networkEmbedLocalCache) Get(networkId server.Id, now time.Time) (entry networkEmbedLocalEntry, ok bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry, ok = self.entries[networkId]
	if ok && !now.Before(entry.expiry) {
		delete(self.entries, networkId)
		return networkEmbedLocalEntry{}, false
	}
	return entry, ok
}

func (self *networkEmbedLocalCache) Put(networkId server.Id, entry networkEmbedLocalEntry) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.entries[networkId]; !ok && networkEmbedLocalCacheMaxSize <= len(self.entries) {
		clear(self.entries)
	}
	self.entries[networkId] = entry
}

func (self *networkEmbedLocalCache) Remove(networkId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.entries, networkId)
}

func (self *networkEmbedLocalCache) Clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	clear(self.entries)
}

var networkEmbedLocal = newNetworkEmbedLocalCache()

// NetworkEmbedEnabled reports whether the network is Embed-enabled, through the
// process cache.
func NetworkEmbedEnabled(ctx context.Context, networkId server.Id) bool {
	now := server.NowUtc()
	if entry, ok := networkEmbedLocal.Get(networkId, now); ok {
		return entry.enabled
	}
	enabled := false
	server.Db(ctx, func(conn server.PgConn) {
		enabled = readNetworkEmbedEnabled(ctx, conn, networkId)
	})
	networkEmbedLocal.Put(networkId, networkEmbedLocalEntry{
		enabled: enabled,
		expiry:  now.Add(networkEmbedLocalCacheTtl),
	})
	return enabled
}

// Testing_EnableNetworkEmbed enables Embed for a test network directly, without
// the existence check, and refreshes this process's cache.
func Testing_EnableNetworkEmbed(ctx context.Context, networkId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_embed (
					network_id,
					enable_time
				)
				VALUES ($1, $2)
				ON CONFLICT (network_id) DO NOTHING
			`,
			networkId,
			server.NowUtc(),
		))
	})
	networkEmbedLocal.Remove(networkId)
}

// Testing_ClearNetworkEmbedCache drops this process's cached flags, so a test
// observes a row written directly to the db.
func Testing_ClearNetworkEmbedCache() {
	networkEmbedLocal.Clear()
}
