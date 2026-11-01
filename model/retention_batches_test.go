package model

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// An id minted at t (server.NewId leads with its creation millisecond).
func retentionTestIdAt(t time.Time) server.Id {
	id := server.ContractIdFloor(t)
	if _, err := rand.Read(id[6:]); err != nil {
		panic(err)
	}
	return id
}

func retentionTestCount(t testing.TB, sql string, args ...any) (count int) {
	t.Helper()
	server.Db(t.Context(), func(conn server.PgConn) {
		server.Raise(conn.QueryRow(t.Context(), sql, args...).Scan(&count))
	})
	return
}

// A time-ordered retention drains in batches of its size until a short batch,
// stops with more when its deadline passed, never deletes a row newer than the
// cutoff, and counts what it deleted under its table.
func TestTimeOrderedRetentionDrainsInBatches(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		userId := server.NewId()
		insert := func(at time.Time) {
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO network_user_action_attempt (network_user_action_attempt_id, user_id, action, create_time)
					VALUES ($1, $2, 'test_action', $3)
				`, retentionTestIdAt(at), userId, at))
			})
		}
		for i := range 5 {
			insert(now.Add(-72*time.Hour - time.Duration(i)*time.Minute))
		}
		insert(now.Add(-time.Hour))
		countRows := func() int {
			return retentionTestCount(t, `SELECT count(*) FROM network_user_action_attempt WHERE user_id = $1`, userId)
		}
		before := testutil.ToFloat64(retentionDeletedRowsCounter.WithLabelValues(RetentionTableNetworkUserActionAttempt))
		minTime := now.Add(-AccountActionAttemptRetention)

		removedCount, more := accountActionAttemptRetention.RemoveBefore(ctx, minTime, 2, time.Now().Add(-time.Second))
		if removedCount != 2 || !more {
			t.Fatalf("a run past its deadline removed %d more=%t, want one batch of 2 and more", removedCount, more)
		}
		removedCount, more = accountActionAttemptRetention.RemoveBefore(ctx, minTime, 2, time.Now().Add(time.Minute))
		if removedCount != 3 || more {
			t.Fatalf("a budgeted run removed %d more=%t, want the remaining 3", removedCount, more)
		}
		if count := countRows(); count != 1 {
			t.Fatalf("%d attempts left, want the hour-old one", count)
		}
		if moved := testutil.ToFloat64(retentionDeletedRowsCounter.WithLabelValues(RetentionTableNetworkUserActionAttempt)) - before; moved != 5 {
			t.Fatalf("the deleted row counter moved by %v, want 5", moved)
		}
	})
}
