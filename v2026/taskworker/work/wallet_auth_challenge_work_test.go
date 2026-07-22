package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func TestRemoveExpiredWalletAuthChallenges(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		result := model.CreateWalletAuthChallenge(model.WalletAuthChallengeArgs{}, ctx)
		connect.AssertEqual(t, result.Error, nil)

		// backdate the row so the cleanup task will remove it
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE wallet_auth_challenge
					SET expire_time = $1
					WHERE challenge_value = $2
				`,
				server.NowUtc().Add(-25*time.Hour).UTC(),
				result.Challenge,
			))
		})

		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		_, err := RemoveExpiredWalletAuthChallenges(&RemoveExpiredWalletAuthChallengesArgs{}, clientSession)
		connect.AssertEqual(t, err, nil)

		var count int
		server.Tx(ctx, func(tx server.PgTx) {
			row := tx.QueryRow(
				ctx,
				`
					SELECT COUNT(*)
					FROM wallet_auth_challenge
					WHERE challenge_value = $1
				`,
				result.Challenge,
			)
			server.Raise(row.Scan(&count))
		})
		connect.AssertEqual(t, count, 0)
	})
}
