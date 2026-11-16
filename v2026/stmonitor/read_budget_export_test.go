// External tests may observe the actual private read owner without installing
// a fake connection, snapshot, role, or successful admission.
package stmonitor

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// The bridge exists only in test builds and exposes error-only observation of
// the original transaction, plus the owned wait clock for finite budget proof.
func ObserveReadForTest(ctx context.Context, now func() time.Time, wait func(context.Context, time.Duration) error, afterBegin func(context.Context, *pgx.Conn, pgx.Tx) error) context.Context {
	return context.WithValue(ctx, readObservationKey{}, readObservationHooks{now: now, wait: wait, afterBegin: afterBegin})
}
