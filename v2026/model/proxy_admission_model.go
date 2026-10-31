// Request admission shares the existing configuration and entitlement readers,
// but gives their Redis operations the caller's socket deadline as well.
package model

import (
	"context"

	"github.com/urnetwork/server/v2026"
)

// Marks only admission reads; durable configuration and entitlement writers
// retain their existing Redis retry and connection policy.
type proxyAdmissionRedisContextKey struct{}

// Keeps legacy callers without a deadline on their existing Redis boundary.
func proxyAdmissionContext(ctx context.Context) context.Context {
	if _, bounded := ctx.Deadline(); !bounded {
		return ctx
	}
	return context.WithValue(ctx, proxyAdmissionRedisContextKey{}, true)
}

// Reads configuration with the connection budget applied to Redis sockets.
// Expiration raises its cause before a cache miss can fall through to storage.
func GetProxyDeviceConfigForAdmission(ctx context.Context, proxyId server.Id) *ProxyDeviceConfig {
	config := GetProxyDeviceConfig(proxyAdmissionContext(ctx), proxyId)
	server.Raise(ctx.Err())
	return config
}

// Preserves the feature gate's missing-network policy while keeping an expired
// lookup distinct from an allowed or denied feature.
func ProxyFeatureAllowedForAdmission(ctx context.Context, proxyId server.Id, feature string) bool {
	allowed := ProxyFeatureAllowed(proxyAdmissionContext(ctx), proxyId, feature)
	server.Raise(ctx.Err())
	return allowed
}

// These callbacks contain only GET or same-slot EVAL. The deadline pool omits
// ordinary retries and detached cluster policy discovery; no preflight is
// needed before the authoritative command that already reports its result.
func proxyAdmissionRedis(ctx context.Context, read func(server.RedisClient)) {
	if admission, _ := ctx.Value(proxyAdmissionRedisContextKey{}).(bool); admission {
		server.Raise(server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
			read(client)
			return nil
		}, server.OptRedisNoPreflight()))
		return
	}
	server.Redis(ctx, read)
}
