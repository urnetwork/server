// Dependency boundaries retain why an operation stopped as well as the legacy
// done sentinel. Observers can distinguish cancellation from finite read loss.
package server

import (
	"context"
	"errors"
)

// The original physical failure and caller stop remain independently visible.
// This changes no decision about whether a statement or transaction can replay.
func dbContextDoneCause(ctx context.Context, original error) error {
	return errors.Join(DbContextDoneError, original, ctx.Err())
}
