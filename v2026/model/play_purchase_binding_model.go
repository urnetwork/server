package model

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server/v2026"
)

// Google Play purchase bindings. A purchase started in the app's billing flow
// carries the network as its obfuscatedExternalAccountId. A purchase made
// outside that flow (a Play Store promo code redemption, a purchase from the
// Play Store listing or subscriptions center) carries no account identifiers,
// so nothing in it names a network. The verify endpoint binds such a purchase
// token to the reporting network when that network holds the issued,
// unredeemed welcome offer (controller/play_purchase_binding_controller.go).
// Later tokens of the same subscription (Google's linkedPurchaseToken chain:
// re-signup, upgrade/downgrade, plan conversion) resolve to the same network
// through this table, keyed by token and carrying the chain's root token.

// GetPlayPurchaseBinding is the network a purchase token is bound to, if any,
// and the root token of its chain.
func GetPlayPurchaseBinding(ctx context.Context, purchaseToken string) (networkId server.Id, rootPurchaseToken string, ok bool) {
	return GetPlayPurchaseBindingInConn(nil, ctx, purchaseToken)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func GetPlayPurchaseBindingInConn(connOwner server.PgConn, ctx context.Context, purchaseToken string) (networkId server.Id, rootPurchaseToken string, ok bool) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		err := conn.QueryRow(
			ctx,
			`
				SELECT network_id, root_purchase_token
				FROM play_purchase_binding
				WHERE purchase_token = $1
			`,
			purchaseToken,
		).Scan(&networkId, &rootPurchaseToken)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
		ok = true
	})
	return
}

func GetPlayPurchaseBindingInTx(tx server.PgTx, ctx context.Context, purchaseToken string) (networkId server.Id, rootPurchaseToken string, ok bool) {
	err := tx.QueryRow(
		ctx,
		`
			SELECT network_id, root_purchase_token
			FROM play_purchase_binding
			WHERE purchase_token = $1
		`,
		purchaseToken,
	).Scan(&networkId, &rootPurchaseToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	server.Raise(err)
	ok = true
	return
}

// BindPlayPurchaseInTx records the binding. Returns false when the token is
// already bound (to any network); the existing binding is never changed.
func BindPlayPurchaseInTx(
	tx server.PgTx,
	ctx context.Context,
	purchaseToken string,
	rootPurchaseToken string,
	networkId server.Id,
	offer string,
) bool {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO play_purchase_binding (
				purchase_token,
				root_purchase_token,
				network_id,
				offer,
				bound_at
			)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (purchase_token) DO NOTHING
		`,
		purchaseToken,
		rootPurchaseToken,
		networkId,
		offer,
		server.NowUtc(),
	))
	return tag.RowsAffected() == 1
}

// ResolvePlayPurchaseBinding is the network an unlinked purchase token
// resolves to: its own binding, else the binding of the token Google names as
// its linkedPurchaseToken. With inherit, a token resolved through its linked
// token is bound too (same network, same root), so the next link of the chain
// resolves even when this one is never reported by the app. Google supplies
// linkedPurchaseToken from our authenticated subscriptionsv2 read, so the
// inheritance is not a guess.
func ResolvePlayPurchaseBinding(
	ctx context.Context,
	purchaseToken string,
	linkedPurchaseToken string,
	inherit bool,
) (networkId server.Id, ok bool) {
	return ResolvePlayPurchaseBindingInConn(nil, ctx, purchaseToken, linkedPurchaseToken, inherit)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func ResolvePlayPurchaseBindingInConn(connOwner server.PgConn,
	ctx context.Context,
	purchaseToken string,
	linkedPurchaseToken string,
	inherit bool,
) (networkId server.Id, ok bool) {
	if networkId, _, ok = GetPlayPurchaseBindingInConn(connOwner, ctx, purchaseToken); ok {
		return
	}
	if linkedPurchaseToken == "" || linkedPurchaseToken == purchaseToken {
		return
	}
	linkedNetworkId, rootPurchaseToken, linkedOk := GetPlayPurchaseBindingInConn(connOwner, ctx, linkedPurchaseToken)
	if !linkedOk {
		return
	}
	if !inherit {
		return linkedNetworkId, true
	}
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		BindPlayPurchaseInTx(tx, ctx, purchaseToken, rootPurchaseToken, linkedNetworkId, "")
		// a concurrent inherit may have won; either way the row is now the answer
		networkId, _, ok = GetPlayPurchaseBindingInTx(tx, ctx, purchaseToken)
	})
	return
}
