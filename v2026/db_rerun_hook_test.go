// Pins where the test-only rerun hook runs, on the pgx wire protocol: between
// two attempts of one transaction, and nowhere else.
package server

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The hook runs after an attempt that failed at a statement or at its commit
// and before the rerun; it does not run for a transaction that commits at
// once or ends with a permanent failure.
func TestTxRerunHookRunsBetweenAttempts(t *testing.T) {
	for _, c := range []struct {
		name  string
		query string
		code  string
		want  []string
	}{
		{name: "statement", query: syntheticEffectStatement, code: "40001", want: []string{"attempt 1", "hook", "attempt 2"}},
		{name: "commit", query: "commit", code: "40001", want: []string{"attempt 1", "hook", "attempt 2"}},
		{name: "permanent", query: syntheticEffectStatement, code: "23502", want: []string{"attempt 1"}},
		{name: "committed", query: "", code: "", want: []string{"attempt 1"}},
	} {
		failed := atomic.Bool{}
		_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
			fixture.queryError = func(connectionIndex int, query string) *pgproto3.ErrorResponse {
				if c.query != "" && query == c.query && failed.CompareAndSwap(false, true) {
					return syntheticPgError(c.code, "")
				}
				return nil
			}
		})
		events := []string{}
		ctx := Testing_WithTxRerunHook(context.Background(), func() {
			events = append(events, "hook")
		})
		attempt := 0
		captureDbErrorPanic(func() {
			txWithPool(ctx, pool, func(tx PgTx) {
				attempt += 1
				events = append(events, fmt.Sprintf("attempt %d", attempt))
				RaisePgResult(tx.Exec(ctx, syntheticEffectStatement))
			})
		})
		if !slices.Equal(events, c.want) {
			t.Errorf("%s: events = %v, want %v", c.name, events, c.want)
		}
	}
}
