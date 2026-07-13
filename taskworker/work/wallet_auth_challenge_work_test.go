package work

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestRemoveExpiredWalletAuthChallenges(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		result := model.CreateWalletAuthChallenge(model.WalletAuthChallengeArgs{}, ctx)
		assert.Equal(t, result.Error, nil)

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
		assert.Equal(t, err, nil)

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
		assert.Equal(t, count, 0)
	})
}
