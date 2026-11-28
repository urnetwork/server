// Ordinary db migrate ends with the provider earning boundary step. sn is live,
// so no option selects it: an existing boundary is retained unchanged, a missing
// one is prepared from the loaded sn.yml, and no sn.yml prepares nothing.
package main

import (
	"context"
	"errors"
	"io"

	"github.com/urnetwork/server/v2026"
)

// Migrations run first and the boundary step reads the migrated schema. A
// canceled owner stops before each step; only a boundary writes its line.
func migrateWithPayoutBoundary(ctx context.Context, output io.Writer, migrate func(context.Context), ensure func(context.Context) (*server.ProviderEarningBoundary, bool, error)) error {
	if ctx == nil {
		return errors.New("database migration requires context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	migrate(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	binding, prepared, err := ensure(ctx)
	if err != nil || binding == nil {
		return err
	}
	return server.WriteProviderEarningBoundary(output, binding, prepared)
}
