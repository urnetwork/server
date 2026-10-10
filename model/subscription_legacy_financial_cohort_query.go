// Bounded cohorts send typed text parameters in one unnamed exchange. This
// avoids cold prepare waits without changing SQL, locks or statement snapshots.
package model

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

type legacyFinancialCohortObservationKey struct{}

// A qualification context can retain finite acquire/transaction phases. These
// are wall observations, not PostgreSQL execution or row-lock measurements.
type legacyFinancialCohortObservation struct {
	WallNs   int64           `json:"wall_ns"`
	Database server.DbTiming `json:"database"`
}

// Ordinary callers allocate no timing state. Qualification observes only the
// existing transaction's joined completion and never adds another query.
func observeLegacyFinancialCohort(ctx context.Context) (*server.DbTiming, func()) {
	observe, _ := ctx.Value(legacyFinancialCohortObservationKey{}).(func(legacyFinancialCohortObservation))
	if observe == nil {
		return nil, func() {}
	}
	timing := &server.DbTiming{}
	started := time.Now()
	return timing, func() {
		observe(legacyFinancialCohortObservation{WallNs: time.Since(started).Nanoseconds(), Database: *timing})
	}
}

// Every caller retains an explicit uuid[] cast. pgx can infer []string text
// encoding before parsing SQL; the custom []server.Id type has no such default.
func queryLegacyFinancialCohort(ctx context.Context, tx server.PgTx, sql string, ids []server.Id, trailing ...any) (pgx.Rows, error) {
	encodedIds := make([]string, len(ids))
	for index, id := range ids {
		encodedIds[index] = id.String()
	}
	args := make([]any, 0, 2+len(trailing))
	args = append(args, pgx.QueryExecModeExec, encodedIds)
	args = append(args, trailing...)
	return tx.Query(ctx, sql, args...)
}
