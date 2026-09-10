package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// The price tier a subscription row was sold at (subscription_renewal.price_tier),
// recorded by every purchase path from now on. The SubscriptionRenewal struct and
// its insert are shared with other in-flight work, so the tier is stamped by a
// separate UPDATE keyed on the row's identity rather than by widening the insert.

// SetSubscriptionRenewalPriceTierByTransactionInTx stamps the tier on the renewal
// rows of one store transaction (Stripe invoice id, App Store transaction id,
// Solana payment reference). Returns the number of rows stamped.
func SetSubscriptionRenewalPriceTierByTransactionInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	market SubscriptionMarket,
	transactionId string,
	tier string,
) int64 {
	if transactionId == "" || tier == "" {
		return 0
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE subscription_renewal
			SET price_tier = $4
			WHERE network_id = $1 AND market = $2 AND transaction_id = $3
		`,
		networkId,
		market,
		transactionId,
		tier,
	))
	return tag.RowsAffected()
}

// SetSubscriptionRenewalPriceTierByPurchaseTokenInTx stamps the tier on the
// renewal row of a Play purchase token ending at endTime.
func SetSubscriptionRenewalPriceTierByPurchaseTokenInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	market SubscriptionMarket,
	purchaseToken string,
	endTime time.Time,
	tier string,
) int64 {
	if purchaseToken == "" || tier == "" {
		return 0
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE subscription_renewal
			SET price_tier = $5
			WHERE network_id = $1 AND market = $2 AND purchase_token = $3 AND end_time = $4
		`,
		networkId,
		market,
		purchaseToken,
		endTime,
		tier,
	))
	return tag.RowsAffected()
}

// SetSubscriptionRenewalPriceTierByTransaction is the transaction variant in its
// own tx.
func SetSubscriptionRenewalPriceTierByTransaction(
	ctx context.Context,
	networkId server.Id,
	market SubscriptionMarket,
	transactionId string,
	tier string,
) (stamped int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		stamped = SetSubscriptionRenewalPriceTierByTransactionInTx(tx, ctx, networkId, market, transactionId, tier)
	}, server.TxReadCommitted)
	return
}

// GetLatestSubscriptionPriceTier is the tier of the network's most recent
// subscription row that carries one, "" when none does.
func GetLatestSubscriptionPriceTier(ctx context.Context, networkId server.Id) (tier string) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT price_tier
				FROM subscription_renewal
				WHERE network_id = $1 AND price_tier IS NOT NULL
				ORDER BY start_time DESC
				LIMIT 1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&tier))
			}
		})
	})
	return
}

// PriceTierForPrice recovers the tier a quoted price belongs to: the tier whose
// plan price (or welcome-offer price, at the configured discount) matches within
// a cent. Used where the purchase record carries a price but no tier (a Solana
// intent). "" when no tier matches.
func (c *ProConfig) PriceTierForPrice(plan string, priceUsd float64, offerPercentOff int) string {
	const tolerance = 0.011
	for _, tier := range c.PriceTiers() {
		regular := tier.PriceUsd(plan)
		if regular <= 0 {
			continue
		}
		if abs(regular-priceUsd) < tolerance {
			return tier.Name
		}
		if 0 < offerPercentOff && offerPercentOff < 100 {
			discounted := roundCents(regular * float64(100-offerPercentOff) / 100)
			if abs(discounted-priceUsd) < tolerance {
				return tier.Name
			}
		}
	}
	return ""
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func roundCents(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
