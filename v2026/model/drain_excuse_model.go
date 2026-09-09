package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Drain excuse markers (CONNECTDRAIN2.md §3.1). Written only by the server's
// own drain / migrate paths as it evicts or migrates a client, and consumed
// exactly once by the announce new-connection branch, so a drain-caused
// reconnect is recorded as excused (`ConnectionExcusedNewCount`) instead of
// invalidating the client's reliability block. A provider never sees or sets
// this key, so it cannot self-excuse; the ttl bounds a stale marker to one
// excuse window.

func drainExcuseKey(clientId server.Id) string {
	return "drain_excuse_" + clientId.String()
}

func drainProvideChangeExcuseKey(clientId server.Id) string {
	return "drain_excuse_pc_" + clientId.String()
}

// how long after a consumed drain excuse the client's provide key changes are
// excused as the mechanical re-announce. Two blocks covers the re-announce
// landing in the consume block or the next, for every announce of the client
// — including the old connection of a make-before-break overlap
const DrainProvideChangeExcuseTtl = 2 * ReliabilityBlockDuration

// SetDrainExcuse marks the client's next reconnect as drain-caused.
// The resident id is stored for observability of which resident was drained.
// Best-effort: a redis error only loses the excuse,
// and the reconnect is charged as organic.
// The provide-change excuse window is armed here at write time (not at
// consume): the mechanical provide re-announce can land before the new
// connection's announce consumes the marker, and the old connection of a
// make-before-break overlap syncs concurrently — every announce of the client
// must observe the window for the whole drain-to-reconnect span.
func SetDrainExcuse(ctx context.Context, clientId server.Id, residentId server.Id, ttl time.Duration) {
	server.HandleError(func() {
		server.Redis(ctx, func(r server.RedisClient) {
			r.Set(ctx, drainExcuseKey(clientId), residentId.String(), ttl)
			r.Set(ctx, drainProvideChangeExcuseKey(clientId), "1", ttl+DrainProvideChangeExcuseTtl)
		})
	})
}

// TakeDrainExcuse consumes the client's drain excuse marker (getdel: consumed
// exactly once). Returns the drained resident id, or nil when no marker is
// set. Best-effort: a redis error is treated as no excuse.
func TakeDrainExcuse(ctx context.Context, clientId server.Id) (residentId *server.Id) {
	server.HandleError(func() {
		server.Redis(ctx, func(r server.RedisClient) {
			value, err := r.GetDel(ctx, drainExcuseKey(clientId)).Result()
			if err != nil {
				// includes the no-marker case (redis.Nil)
				return
			}
			if id, parseErr := server.ParseId(value); parseErr == nil {
				residentId = &id
			}
		})
	})
	return
}

// HasDrainExcuse reports whether the client currently has an unconsumed drain
// excuse marker, without consuming it
func HasDrainExcuse(ctx context.Context, clientId server.Id) (has bool) {
	server.HandleError(func() {
		server.Redis(ctx, func(r server.RedisClient) {
			count, err := r.Exists(ctx, drainExcuseKey(clientId)).Result()
			if err != nil {
				return
			}
			has = 0 < count
		})
	})
	return
}

// HasDrainProvideChangeExcuse reports whether the client is inside the
// provide-change excuse window armed by a consumed drain excuse. Read-only
// and ttl-bounded: every announce of the client observes the same window (the
// old connection of a make-before-break overlap included). Best-effort: a
// redis error is treated as no excuse.
func HasDrainProvideChangeExcuse(ctx context.Context, clientId server.Id) (has bool) {
	server.HandleError(func() {
		server.Redis(ctx, func(r server.RedisClient) {
			count, err := r.Exists(ctx, drainProvideChangeExcuseKey(clientId)).Result()
			if err != nil {
				return
			}
			has = 0 < count
		})
	})
	return
}
