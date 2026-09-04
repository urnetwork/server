package model

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
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
// Short-ttl by design: the persisted identities are only useful while the
// provider-side flow state survives (udp 60s idle, tcp 300s), and a stale
// snapshot must not linger. The jwt is client-scoped, matching the
// sensitivity of the proxy_client json already stored.

// proxyWindowIdentityTtl bounds how long a persisted window snapshot is
// restorable. It comfortably covers a deploy restart; beyond it the provider
// flow timers have evicted anything resumable anyway.
const proxyWindowIdentityTtl = 10 * time.Minute

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
