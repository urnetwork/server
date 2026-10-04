package model

import (
	"context"

	"github.com/urnetwork/server"
)

// Preserve the baseline's test body and forward the new dispatch observation.
func negativeDispatchLookup(c *subscriberNegativeCache, ctx context.Context, ids []server.Id, read func(context.Context, []server.Id, func()) (map[server.Id]bool, error)) (map[server.Id]bool, error) {
	return c.lookup(ctx, ids, read)
}
