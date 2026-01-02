package model

import (
	"context"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

/**
 * Account redemption codes allow users to redeem special offers or credits to their accounts.
 * Currently, created in the AppSumo webhook
 */

func CreateAccountRedemptionCode(ctx context.Context) (redemptionCode string, returnErr error) {

	server.Tx(ctx, func(tx server.PgTx) {

		redemptionCode = generateAlphanumericCode(8)

		_, err := tx.Exec(
			ctx,
			`
				INSERT INTO account_redemption_code (
					code
				)
				VALUES ($1)
			`,
			redemptionCode,
		)

		if err != nil {
			glog.Infof("Error creating account redemption code: %v", err)
			returnErr = err
		}

	})

	return

}

func ClaimAccountRedemptionCode(
	code string,
	session *session.ClientSession,
) (claimed bool, err error) {

	server.Tx(session.Ctx, func(tx server.PgTx) {

		result, execErr := tx.Exec(
			session.Ctx,
			`
				UPDATE account_redemption_code
				SET network_id = $2
				WHERE code = $1 AND network_id IS NULL
			`,
			code,
			session.ByJwt.NetworkId,
		)

		if execErr != nil {
			err = execErr
			return
		}

		// check if a row was updated
		if result.RowsAffected() == 1 {
			claimed = true
		} else {
			claimed = false // already claimed or code doesn't exist
		}

		// todo - apply credits or bonus here

	})

	return

}
