package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// Wg endpoint handoff store (PROXYDRAIN1.md §3.4).
//
// At drain end the old proxy instance exports each active wg peer's learned
// endpoint; the replacement instance reads it, seeds the endpoints on its
// restored peers, and initiates handshakes from the server side — so the
// clients never wait out their own dead-session timers.
//
// Server-written, server-read, short-ttl, per (host, block): a stale entry
// costs at most one wasted initiation to an endpoint the client no longer
// holds (the client's own retry path is unaffected).

// defaultProxyWgHandoffTtl guards a non-positive caller ttl. The operative
// ttl is passed in by the caller (`ProxySettings.WgHandoffRequestTtl`): it
// must cover the whole deploy window — the replacement publishes its
// generation request at boot and the old instance reads it at drain end,
// after the host's serialized block drains (blocks x grace), plus margin.
const defaultProxyWgHandoffTtl = 10 * time.Minute

func proxyWgHandoffTtlOrDefault(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return defaultProxyWgHandoffTtl
	}
	return ttl
}

func proxyWgHandoffBaseKey(proxyHost string, block string) string {
	return fmt.Sprintf("{pwh_%s_%s}", proxyHost, block)
}

func proxyWgHandoffKey(proxyHost string, block string, generation string) string {
	key := proxyWgHandoffBaseKey(proxyHost, block)
	if generation != "" {
		key += ":" + generation
	}
	return key
}

func proxyWgHandoffRequestKey(proxyHost string, block string) string {
	return proxyWgHandoffBaseKey(proxyHost, block) + ":request"
}

type ProxyWgHandoffPeer struct {
	ClientIpv4 netip.Addr `json:"client_ipv4"`
	// Endpoint is the peer's last known endpoint, "host:port"
	Endpoint          string    `json:"endpoint"`
	LastHandshakeTime time.Time `json:"last_handshake_time"`
}

type proxyWgHandoff struct {
	Generation string                `json:"generation,omitempty"`
	ExportTime time.Time             `json:"export_time"`
	Peers      []*ProxyWgHandoffPeer `json:"peers"`
}

// BeginProxyWgHandoff publishes the replacement instance's generation before
// traffic is redirected and the old instance is drained. The old instance
// tags its later export with this generation, so a replacement cannot consume
// stale data from a prior deploy. `ttl` must outlive the replacement's poll
// budget (the request is read by the old instance at its drain end, which on
// a multi-block host can be blocks x grace later).
func BeginProxyWgHandoff(
	ctx context.Context,
	proxyHost string,
	block string,
	generation string,
	ttl time.Duration,
) {
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(
			ctx,
			proxyWgHandoffRequestKey(proxyHost, block),
			generation,
			proxyWgHandoffTtlOrDefault(ttl),
		).Err())
	})
}

func CurrentProxyWgHandoffGeneration(
	ctx context.Context,
	proxyHost string,
	block string,
) (generation string) {
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		generation, err = r.Get(ctx, proxyWgHandoffRequestKey(proxyHost, block)).Result()
		if err == redis.Nil {
			generation = ""
			return
		}
		server.Raise(err)
	})
	return
}

// SetProxyWgHandoff stores the exported wg peer endpoints for (host, block),
// replacing any previous export.
func SetProxyWgHandoff(
	ctx context.Context,
	proxyHost string,
	block string,
	exportTime time.Time,
	peers []*ProxyWgHandoffPeer,
	ttl time.Duration,
) {
	SetProxyWgHandoffForGeneration(ctx, proxyHost, block, "", exportTime, peers, ttl)
}

func SetProxyWgHandoffForGeneration(
	ctx context.Context,
	proxyHost string,
	block string,
	generation string,
	exportTime time.Time,
	peers []*ProxyWgHandoffPeer,
	ttl time.Duration,
) {
	handoff := &proxyWgHandoff{
		Generation: generation,
		ExportTime: exportTime,
		Peers:      peers,
	}
	handoffJson, err := json.Marshal(handoff)
	server.Raise(err)

	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(
			ctx,
			proxyWgHandoffKey(proxyHost, block, generation),
			string(handoffJson),
			proxyWgHandoffTtlOrDefault(ttl),
		).Err())
	})
}

// TakeProxyWgHandoff reads AND removes the exported wg peer endpoints for
// (host, block). Consumed exactly once (getdel): a second replacement
// instance (e.g. a crash loop) must not re-initiate from a stale export.
// Returns the export time and peers, or a zero time and nil when absent.
func TakeProxyWgHandoff(
	ctx context.Context,
	proxyHost string,
	block string,
) (time.Time, []*ProxyWgHandoffPeer) {
	return TakeProxyWgHandoffForGeneration(ctx, proxyHost, block, "")
}

func TakeProxyWgHandoffForGeneration(
	ctx context.Context,
	proxyHost string,
	block string,
	generation string,
) (time.Time, []*ProxyWgHandoffPeer) {
	var handoffJson string
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		handoffJson, err = r.GetDel(ctx, proxyWgHandoffKey(proxyHost, block, generation)).Result()
		if err == redis.Nil {
			handoffJson = ""
			return
		}
		server.Raise(err)
	})
	if handoffJson == "" {
		return time.Time{}, nil
	}
	var handoff proxyWgHandoff
	if err := json.Unmarshal([]byte(handoffJson), &handoff); err != nil {
		return time.Time{}, nil
	}
	if handoff.Generation != generation {
		return time.Time{}, nil
	}
	return handoff.ExportTime, handoff.Peers
}
