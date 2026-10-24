package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// A Google or Apple account keeps its email only in the legacy
// network_user.user_auth column and has no password sign-in. The reset code
// reaches it, but setting a password must not report success while nothing
// changed (inbox 7969, urnetwork/android#485).
func TestAuthPasswordSetRefusesAccountWithoutPasswordAuth(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "127.0.0.1:0", nil)
		defer clientSession.Cancel()

		userId := server.NewId()
		networkId := server.NewId()
		userAuth := fmt.Sprintf("sso-%s@bringyour.com", networkId)

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO network_user (user_id, user_name, auth_type, user_auth)
				 VALUES ($1, $2, $3, $4)`,
				userId, "sso-no-password", AuthTypeGoogle, userAuth,
			))
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO network (network_id, network_name, admin_user_id)
				 VALUES ($1, $2, $3)`,
				networkId, fmt.Sprintf("ssonopw%s", networkId), userId,
			))
		})

		reset, err := AuthPasswordResetCreateCode(
			AuthPasswordResetCreateCodeArgs{UserAuth: userAuth},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, reset.ResetCode, nil)

		result, err := AuthPasswordSet(
			AuthPasswordSetArgs{
				ResetCode: *reset.ResetCode,
				Password:  "new-password",
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)
		connect.AssertNotEqual(t, result.Error, nil)
		connect.AssertEqual(t, result.Error.Message, AuthPasswordSetNoPasswordAuthMessage)

		// the code is left unused
		var used bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx,
				`SELECT used FROM user_auth_reset WHERE user_id = $1 AND reset_code = $2`,
				userId, *reset.ResetCode,
			).Scan(&used))
		})
		connect.AssertEqual(t, used, false)
	})
}
