// Compatibility names for Connect transport settings whose implementation is
// owned by the parent server package.
package connect

import (
	"context"

	"github.com/urnetwork/server"
)

type ConnectionRateLimitSettings = server.ConnectionRateLimitSettings
type ConnectionRateLimit = server.ConnectionRateLimit

// Returns the canonical production settings.
func DefaultConnectionRateLimitSettings() *ConnectionRateLimitSettings {
	return server.DefaultConnectionRateLimitSettings()
}

// Creates canonical Connect admission state with production settings.
func NewConnectionRateLimitWithDefaults(
	ctx context.Context,
	clientAddress string,
	handlerId server.Id,
) (*ConnectionRateLimit, error) {
	return server.NewConnectionRateLimitWithDefaults(ctx, clientAddress, handlerId)
}

// Creates canonical Connect admission state with explicit settings.
func NewConnectionRateLimit(
	ctx context.Context,
	clientAddress string,
	handlerId server.Id,
	settings *ConnectionRateLimitSettings,
) (*ConnectionRateLimit, error) {
	return server.NewConnectionRateLimit(ctx, clientAddress, handlerId, settings)
}
