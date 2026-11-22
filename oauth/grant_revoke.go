package oauth

// A credential rotation ends the user's grants.
//
// A password reset, a sign-out-all and an account handed to the verifier of
// its address each set the user's credential_change_time, and every token
// created before it is refused. The applications the user approved before it
// end too: in the rotation's transaction the remembered consents go, so each
// application asks again; unredeemed codes go; and every refresh token is
// revoked. Access tokens already issued run out within AccessTokenDuration.
//
// The rotation holds the network's session lifecycle lock exclusively, and an
// approval holds it shared (Authorize), so an approval either commits before
// the rotation and is revoked by it, or waits and is refused. A code exchange
// or a refresh that runs across the rotation can still write a refresh token
// after it; each grant keeps the time it was approved, so that token is
// refused at its first use, as is every code and refresh token of a grant
// approved before the rotation (RedeemAuthorizationCode, RotateRefreshToken).

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// Ends every grant of the user, in the caller's transaction, which holds the
// session lifecycle lock of the user's network exclusively. Returns how many
// approved applications it disconnected.
func RevokeUserGrantsInTx(ctx context.Context, tx server.PgTx, userId server.Id, revokeTime time.Time) (revokedGrantCount int64) {
	consentTag := server.RaisePgResult(tx.Exec(
		ctx,
		`
		DELETE FROM oauth_consent
		WHERE user_id = $1
		`,
		userId,
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		DELETE FROM oauth_authorization_code
		WHERE user_id = $1 AND redeem_time IS NULL
		`,
		userId,
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		UPDATE oauth_refresh_token
		SET revoke_time = $2
		WHERE user_id = $1 AND revoke_time IS NULL
		`,
		userId,
		revokeTime,
	))
	return consentTag.RowsAffected()
}

// Reports whether a credential or grant of the user dated credentialTime is
// still current: the user exists, and no credential rotation came after it.
// The same rule refuses a token created before the rotation
// (session.ValidateByJwtState).
func credentialCurrentInTx(ctx context.Context, tx server.PgTx, userId server.Id, credentialTime time.Time) bool {
	var credentialChangeTime time.Time
	found := false
	result, err := tx.Query(
		ctx,
		`
		SELECT credential_change_time
		FROM network_user
		WHERE user_id = $1
		`,
		userId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			found = true
			server.Raise(result.Scan(&credentialChangeTime))
		}
	})
	return found && !credentialTime.Before(credentialChangeTime)
}
