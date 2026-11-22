// One physical lease covers ownership and its financial transaction.
package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The wire pool has one slot. Any nested checkout deadlocks against the finite
// context; successful posts must run only after that exact slot is returned.
func TestOwnedTxReusesAdmittedCheckoutAndReleasesBeforePosts(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, nil,
		func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) { fixture.queryRows = ownedTransactionWireRows })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	acquires, callbacks, posts := 0, 0, 0
	ownedTxWithResource(ctx, []PgOwnershipKey{NewPgOwnershipKey("owned-tx-reuse-test", NewId())},
		ownedTransactionWireResource(), func(ctx context.Context) (PgConn, error) {
			acquires++
			return pool.open().Acquire(ctx)
		}, func(tx PgTx) {
			callbacks++
			if stats := pool.open().Stat(); stats.AcquiredConns() != 1 || stats.TotalConns() != 1 {
				t.Fatal("business callback did not retain exactly its admitted physical lease")
			}
			RaisePgResult(tx.Exec(ctx, `INSERT INTO synthetic_effect VALUES(1)`))
			AddTxPostCommit(tx, "owned-reuse", func() any {
				posts++
				if pool.open().Stat().AcquiredConns() != 0 {
					t.Fatal("post retained the business checkout")
				}
				return nil
			})
		}, TxReadCommitted)
	if acquires != 1 || callbacks != 1 || posts != 1 || pool.open().Stat().AcquiredConns() != 0 {
		t.Fatalf("owned transaction changed exact checkout lifetime: acquires=%d callbacks=%d posts=%d", acquires, callbacks, posts)
	}
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	begins, commits, unlocks := 0, 0, 0
	for _, query := range fixture.queries {
		switch {
		case strings.HasPrefix(query, "begin"):
			begins++
		case query == "commit":
			commits++
		case query == "SELECT pg_advisory_unlock_all()":
			unlocks++
		}
	}
	if fixture.dialCount != 1 || begins != 1 || commits != 1 || unlocks != 1 {
		t.Fatalf("owned transaction opened another physical/transactional owner: dials=%d begins=%d commits=%d unlocks=%d", fixture.dialCount, begins, commits, unlocks)
	}
}
