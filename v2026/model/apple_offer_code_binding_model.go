package model

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server/v2026"
)

// App Store offer-code bindings. An offer code redeemed through the App Store
// redeem sheet or link carries no appAccountToken, so nothing in the
// transaction names a network. The verify endpoint binds such a transaction's
// subscription (its original transaction id) to the reporting network when that
// network holds the issued, unredeemed welcome offer code
// (controller/apple_offer_code_binding_controller.go). Renewals of the same
// subscription then resolve to the same network through this table.

// GetAppleOfferCodeBindingNetworkId is the network an original transaction id is
// bound to, if any.
func GetAppleOfferCodeBindingNetworkId(ctx context.Context, originalTransactionId string) (networkId server.Id, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		err := conn.QueryRow(
			ctx,
			`
				SELECT network_id
				FROM apple_offer_code_binding
				WHERE original_transaction_id = $1
			`,
			originalTransactionId,
		).Scan(&networkId)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
		ok = true
	})
	return
}

func GetAppleOfferCodeBindingNetworkIdInTx(tx server.PgTx, ctx context.Context, originalTransactionId string) (networkId server.Id, ok bool) {
	err := tx.QueryRow(
		ctx,
		`
			SELECT network_id
			FROM apple_offer_code_binding
			WHERE original_transaction_id = $1
		`,
		originalTransactionId,
	).Scan(&networkId)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	server.Raise(err)
	ok = true
	return
}

// BindAppleOfferCodeTransactionInTx records the binding. Returns false when the
// original transaction id is already bound (to any network); the existing
// binding is never changed.
func BindAppleOfferCodeTransactionInTx(
	tx server.PgTx,
	ctx context.Context,
	originalTransactionId string,
	transactionId string,
	networkId server.Id,
	offerIdentifier string,
) bool {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO apple_offer_code_binding (
				original_transaction_id,
				network_id,
				transaction_id,
				offer_identifier,
				bound_at
			)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (original_transaction_id) DO NOTHING
		`,
		originalTransactionId,
		networkId,
		transactionId,
		offerIdentifier,
		server.NowUtc(),
	))
	return tag.RowsAffected() == 1
}

// GetOnboardingOfferForUpdateInTx reads the network's offer and locks the row
// for the rest of the tx, so two reports cannot both redeem it.
func GetOnboardingOfferForUpdateInTx(tx server.PgTx, ctx context.Context, networkId server.Id) (offer *OnboardingOffer) {
	result, err := tx.Query(ctx, onboardingOfferSelect+" FOR UPDATE", networkId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			offer = scanOnboardingOffer(result)
		}
	})
	return
}
