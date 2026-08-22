package model

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// Proxy client activity set (PROXYDRAIN1.md §3.3).
//
// Each proxy instance periodically flushes the proxy ids of its recently
// active devices into a per-(host, block) redis sorted set, scored by the
// activity time. A replacement instance starting during a deploy reads the
// set to pre-warm devices for the clients that were active on the old
// instance, so the first packet after the cutover hits a warm device instead
// of paying the cold start (db + platform dial + egress window).
//
// The set self-reaps: every touch trims entries older than the retention and
// refreshes the key ttl, so an abandoned (host, block) key disappears on its
// own.

// proxyClientActivityRetention bounds how far back the activity set reaches.
// It must cover any PrewarmActivityWindow with margin; entries older than
// this are trimmed on every touch.
const proxyClientActivityRetention = 1 * time.Hour

// proxyClientActivityTtl is the key ttl, refreshed on every touch. It exceeds
// the retention so a live key never expires between touches.
const proxyClientActivityTtl = 2 * time.Hour

// one key per (host, block); the hash tag keeps any future multi-key use in
// one cluster slot (see the redis pipeline hash-tag rule)
func proxyClientActivityKey(proxyHost string, block string) string {
	return fmt.Sprintf("{pca_%s_%s}", proxyHost, block)
}

// TouchProxyClientActivity records activity for proxy clients on
// (host, block) at activityTime, trims entries past the retention, and
// refreshes the key ttl.
func TouchProxyClientActivity(
	ctx context.Context,
	proxyHost string,
	block string,
	activityTime time.Time,
	proxyIds ...server.Id,
) {
	if len(proxyIds) == 0 {
		return
	}

	key := proxyClientActivityKey(proxyHost, block)
	members := make([]redis.Z, 0, len(proxyIds))
	score := float64(activityTime.UnixMilli())
	for _, proxyId := range proxyIds {
		members = append(members, redis.Z{
			Score:  score,
			Member: proxyId.String(),
		})
	}
	trimBefore := activityTime.Add(-proxyClientActivityRetention)

	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.ZAdd(ctx, key, members...).Err())
		server.Raise(r.ZRemRangeByScore(
			ctx,
			key,
			"-inf",
			fmt.Sprintf("(%d", trimBefore.UnixMilli()),
		).Err())
		server.Raise(r.Expire(ctx, key, proxyClientActivityTtl).Err())
	})
}

// GetActiveProxyClients returns the proxy ids recorded active on
// (host, block) within the window ending at now.
func GetActiveProxyClients(
	ctx context.Context,
	proxyHost string,
	block string,
	window time.Duration,
	now time.Time,
) []server.Id {
	key := proxyClientActivityKey(proxyHost, block)
	minScore := fmt.Sprintf("%d", now.Add(-window).UnixMilli())

	proxyIds := []server.Id{}
	server.Redis(ctx, func(r server.RedisClient) {
		members, err := r.ZRangeByScore(ctx, key, &redis.ZRangeBy{
			Min: minScore,
			Max: "+inf",
		}).Result()
		server.Raise(err)
		for _, member := range members {
			if proxyId, err := server.ParseId(member); err == nil {
				proxyIds = append(proxyIds, proxyId)
			}
		}
	})
	return proxyIds
}
