package model

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// Window identity persistence for hosted proxy devices (PROXYDRAIN1.md
// §3.5).
//
// Each hosted device's multi-client window mints ephemeral platform client
// ids; the egress providers' NAT flows are keyed by those ids, so a process
// restart that mints fresh ids orphans every established inner flow. The
// device persists its live (window identity, destination) pairs here, and
// the recreated device reuses them — same client id, jwt, and instance id
// against the same provider — so the flows resume.
//
// The same snapshot lets a proxy whose device closed for idleness reopen on
// the exits it had (P90SPEED.md §4.1, fix 4): a restored identity needs no
// mint, so the reopened window's first exit does not wait on the hosted mint
// admission. A device leaves its identities registered when it closes, and
// restarts the snapshot's ttl then (RefreshProxyWindowIdentities), so the
// ttl counts from the close. A restored identity the platform refuses, or
// whose exit no longer answers, is retired by its window and replaced with a
// fresh mint. The jwt is client-scoped, matching the sensitivity of the
// proxy_client json already stored.

// proxyWindowIdentityTtl bounds how long a persisted window snapshot is
// restorable after its last write or its device's close. Two hours (owner,
// 2026-11-18: "Let reopened proxies reuse their exits for 2 hours instead of
// 10 minutes"): a sweep that opens a proxy every 40-45 minutes, and a
// customer who pauses longer than a device's idle hold, reopen on the exits
// they had. It still covers a deploy restart. Ten minutes, the earlier value,
// expired before almost every reopen: on Main on 2026-11-18 about 0.7% of
// device builds restored an identity.
const proxyWindowIdentityTtl = 2 * time.Hour

// How long a persisted window snapshot is restorable after its last write or
// refresh.
func ProxyWindowIdentityTtl() time.Duration {
	return proxyWindowIdentityTtl
}

func proxyWindowIdentityKey(proxyId server.Id) string {
	return fmt.Sprintf("{pwi_%s}", proxyId)
}

type ProxyWindowClientIdentity struct {
	ClientId   server.Id `json:"client_id"`
	ByJwt      string    `json:"by_jwt"`
	InstanceId server.Id `json:"instance_id"`
	// DestinationIds is the multi-hop destination (intermediaries + final
	// provider client id) this identity dials.
	DestinationIds []server.Id `json:"destination_ids"`
}

// SetProxyWindowIdentities stores the device's live window identity
// snapshot, replacing the previous one. An empty snapshot clears the key.
func SetProxyWindowIdentities(
	ctx context.Context,
	proxyId server.Id,
	identities []*ProxyWindowClientIdentity,
) {
	key := proxyWindowIdentityKey(proxyId)
	if len(identities) == 0 {
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, key).Err())
		})
		return
	}
	identitiesJson, err := json.Marshal(identities)
	server.Raise(err)
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, key, string(identitiesJson), proxyWindowIdentityTtl).Err())
	})
}

// RefreshProxyWindowIdentities restarts the ttl of the proxy's persisted
// window snapshot, if it has one, and reports whether it had one. A device
// calls it when it closes: its identities stay registered, and the snapshot
// written at its window's last change names them, so restarting the ttl
// keeps them restorable for proxyWindowIdentityTtl after the close. It never
// writes identities, so a snapshot a newer device has since replaced or
// cleared is not overwritten with an older one. It runs on the device's close
// path and is only an optimization, so an error is raised at once rather than
// retried: an unanswered refresh leaves the snapshot its earlier ttl.
func RefreshProxyWindowIdentities(
	ctx context.Context,
	proxyId server.Id,
) bool {
	var refreshed bool
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		refreshed, err = r.Expire(ctx, proxyWindowIdentityKey(proxyId), proxyWindowIdentityTtl).Result()
		server.Raise(err)
	}, server.OptNoRetry())
	return refreshed
}

// GetProxyWindowIdentities returns the persisted window identity snapshot,
// or nil when absent. A plain read (not consumed): the restored device
// rewrites the snapshot as its window re-forms, which naturally supersedes
// this one.
func GetProxyWindowIdentities(
	ctx context.Context,
	proxyId server.Id,
) []*ProxyWindowClientIdentity {
	var identitiesJson string
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		identitiesJson, err = r.Get(ctx, proxyWindowIdentityKey(proxyId)).Result()
		if err == redis.Nil {
			identitiesJson = ""
			return
		}
		server.Raise(err)
	})
	if identitiesJson == "" {
		return nil
	}
	var identities []*ProxyWindowClientIdentity
	if err := json.Unmarshal([]byte(identitiesJson), &identities); err != nil {
		return nil
	}
	return identities
}
