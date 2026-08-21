package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestRemoveExpiredAuthAttemptsReschedulesAtTwelveHours(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		if removeExpiredAuthAttemptsInterval != 12*time.Hour {
			t.Fatalf("auth attempt cleanup interval = %v, want 12h", removeExpiredAuthAttemptsInterval)
		}

		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			err := RemoveExpiredAuthAttemptsPost(
				&RemoveExpiredAuthAttemptsArgs{},
				&RemoveExpiredAuthAttemptsResult{},
				clientSession,
				tx,
			)
			if err != nil {
				t.Fatalf("cleanup Post returned %v", err)
			}

			result, err := tx.Query(
				ctx,
				`SELECT run_at FROM pending_task WHERE run_once_key = '["remove_expired_auth_attempts"]'`,
			)
			server.WithPgResult(result, err, func() {
				if !result.Next() {
					t.Fatal("cleanup Post did not schedule the next legacy-table drain")
				}
				var runAt time.Time
				server.Raise(result.Scan(&runAt))
				delay := runAt.Sub(before)
				if delay < removeExpiredAuthAttemptsInterval-time.Minute ||
					removeExpiredAuthAttemptsInterval+time.Minute < delay {
					t.Fatalf("cleanup scheduled %v out, want approximately %v", delay, removeExpiredAuthAttemptsInterval)
				}
			})
		})
	})
}
