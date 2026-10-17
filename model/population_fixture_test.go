// Large population fixtures for plan and scale tests. Each batch is its own
// autocommit statement, so live session triggers stay on while a single setup
// transaction never retains one endpoint fence per inserted client.
package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// The rows one fixture statement inserts.
const populationFixtureBatchSize = 512

// Runs statement once per batch of [first, last] row numbers in 1..count,
// appending first and last to args, and requires every row of the range.
func insertPopulationFixture(t testing.TB, ctx context.Context, conn server.PgConn, count int, statement string, args ...any) {
	t.Helper()
	for first := 1; first <= count; first += populationFixtureBatchSize {
		last := min(first+populationFixtureBatchSize-1, count)
		arguments := append(append([]any(nil), args...), first, last)
		result, err := conn.Exec(ctx, statement, arguments...)
		server.Raise(err)
		if result.RowsAffected() != int64(last-first+1) {
			t.Fatalf("population fixture lost a row range: first=%d last=%d inserted=%d", first, last, result.RowsAffected())
		}
	}
}
