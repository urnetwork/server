package model

// The re-engagement program's win-back offers (network_winback_offer;
// mmm/reengagement/PLAN.md "THE STORES" 6): issued by the W1 step, read by
// every purchase path of the offer's store, shown on the plan response while
// active, and stamped redeemed by the purchase that used it. One offer per
// network per 365 days, whatever became of the last one.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/reengagement"
)

type WinbackOffer struct {
	OfferId    server.Id
	NetworkId  server.Id
	Store      string
	Plan       string
	PercentOff int
	IssuedAt   time.Time
	ExpiresAt  time.Time
	RedeemedAt *time.Time
	// the store's handle: exactly one is set, for the offer's store
	StripePromotionCodeId *string
	PlayOfferTag          *string
	AppleOfferId          *string
}

// Active reports whether the offer can be redeemed at `now`: unredeemed and
// before its expiry.
func (self *WinbackOffer) Active(now time.Time) bool {
	return self != nil && self.RedeemedAt == nil && now.Before(self.ExpiresAt)
}

// WinbackOfferIssueSkew allows for clock skew between a store's purchase
// time and our issued_at, as the Play welcome-offer binding does.
const WinbackOfferIssueSkew = 5 * time.Minute

type IssueWinbackOfferArgs struct {
	NetworkId  server.Id
	Store      string
	Plan       string
	PercentOff int
	// the offer's window, fixed by the caller before any store handle is made
	// for it (a Stripe promotion code expires with the offer)
	IssuedAt  time.Time
	ExpiresAt time.Time
	// the store's handle; empty for the other stores
	StripePromotionCodeId string
	PlayOfferTag          string
	AppleOfferId          string
}

var ErrWinbackNetworkNotFound = errors.New("network does not exist")

// IssueWinbackOffer records the offer unless the network was issued one in the
// reengagement.WinbackOfferInterval before args.IssuedAt; then it returns that
// one (created = false) whatever its state, so a replayed W1 finds its own
// offer and a later one finds none to make. The issue is serialized per
// network (an advisory transaction lock on the network) and holds a key-share
// lock on the network row, so it never records for a network being deleted.
func IssueWinbackOffer(ctx context.Context, args *IssueWinbackOfferArgs) (offer *WinbackOffer, created bool, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		offer, created, returnErr = issueWinbackOfferInTx(tx, ctx, args)
	}, server.TxReadCommitted)
	return
}

func issueWinbackOfferInTx(tx server.PgTx, ctx context.Context, args *IssueWinbackOfferArgs) (*WinbackOffer, bool, error) {
	if !lockOnboardingNetworkInTx(tx, ctx, args.NetworkId) {
		return nil, false, ErrWinbackNetworkNotFound
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"network_winback_offer:"+args.NetworkId.String(),
	))
	if existing := latestWinbackOfferInTx(tx, ctx, args.NetworkId); existing != nil &&
		args.IssuedAt.Before(existing.IssuedAt.Add(reengagement.WinbackOfferInterval)) {
		return existing, false, nil
	}
	offerId := server.NewId()
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_winback_offer (
				offer_id,
				network_id,
				store,
				plan,
				percent_off,
				issued_at,
				expires_at,
				stripe_promotion_code_id,
				play_offer_tag,
				apple_offer_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`,
		offerId,
		args.NetworkId,
		args.Store,
		args.Plan,
		args.PercentOff,
		args.IssuedAt,
		args.ExpiresAt,
		nullIfEmpty(args.StripePromotionCodeId),
		nullIfEmpty(args.PlayOfferTag),
		nullIfEmpty(args.AppleOfferId),
	))
	offer := getWinbackOfferInTx(tx, ctx, offerId)
	if offer == nil {
		return nil, false, errors.New("the issued win-back offer could not be read back")
	}
	return offer, true, nil
}

const winbackOfferColumns = `
	offer_id,
	network_id,
	store,
	plan,
	percent_off,
	issued_at,
	expires_at,
	redeemed_at,
	stripe_promotion_code_id,
	play_offer_tag,
	apple_offer_id
`

func scanWinbackOffer(result pgx.Rows) *WinbackOffer {
	offer := &WinbackOffer{}
	server.Raise(result.Scan(
		&offer.OfferId,
		&offer.NetworkId,
		&offer.Store,
		&offer.Plan,
		&offer.PercentOff,
		&offer.IssuedAt,
		&offer.ExpiresAt,
		&offer.RedeemedAt,
		&offer.StripePromotionCodeId,
		&offer.PlayOfferTag,
		&offer.AppleOfferId,
	))
	return offer
}

const latestWinbackOfferSelect = `
	SELECT ` + winbackOfferColumns + `
	FROM network_winback_offer
	WHERE network_id = $1
	ORDER BY issued_at DESC
	LIMIT 1
`

func latestWinbackOfferInTx(tx server.PgTx, ctx context.Context, networkId server.Id) (offer *WinbackOffer) {
	result, err := tx.Query(ctx, latestWinbackOfferSelect, networkId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			offer = scanWinbackOffer(result)
		}
	})
	return
}

func getWinbackOfferInTx(tx server.PgTx, ctx context.Context, offerId server.Id) (offer *WinbackOffer) {
	result, err := tx.Query(ctx, `SELECT `+winbackOfferColumns+` FROM network_winback_offer WHERE offer_id = $1`, offerId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			offer = scanWinbackOffer(result)
		}
	})
	return
}

// GetLatestWinbackOffer is the network's newest offer in any state, or nil.
func GetLatestWinbackOffer(ctx context.Context, networkId server.Id) (offer *WinbackOffer) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, latestWinbackOfferSelect, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				offer = scanWinbackOffer(result)
			}
		})
	})
	return
}

// GetActiveWinbackOffer is the network's offer when it can be redeemed at
// `now`, else nil. The plan response and every purchase path ask this.
func GetActiveWinbackOffer(ctx context.Context, networkId server.Id, now time.Time) *WinbackOffer {
	if offer := GetLatestWinbackOffer(ctx, networkId); offer.Active(now) {
		return offer
	}
	return nil
}

// GetWinbackOffer reads one offer by id, or nil.
func GetWinbackOffer(ctx context.Context, offerId server.Id) (offer *WinbackOffer) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT `+winbackOfferColumns+` FROM network_winback_offer WHERE offer_id = $1`, offerId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				offer = scanWinbackOffer(result)
			}
		})
	})
	return
}

// SetWinbackOfferStripePromotionCode replaces the Stripe offer's promotion
// code (the first was used up by a checkout that never paid) while the offer
// is still active. Returns false when the offer is redeemed, expired or not a
// Stripe offer.
func SetWinbackOfferStripePromotionCode(ctx context.Context, offerId server.Id, promotionCodeId string, now time.Time) (updated bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_winback_offer
				SET stripe_promotion_code_id = $2
				WHERE offer_id = $1 AND store = $3 AND redeemed_at IS NULL AND $4 < expires_at
			`,
			offerId,
			promotionCodeId,
			reengagement.WinbackStoreStripe,
			now,
		))
		updated = tag.RowsAffected() == 1
	}, server.TxReadCommitted)
	return
}

// RedeemWinbackOfferInTx stamps the network's offer on `store` redeemed by a
// purchase made at `at`: the newest unredeemed offer issued (less the clock
// skew) before `at` that `at` falls before the expiry of. With offerId, only
// that offer. Returns the offer it redeemed, nil when none (a renewal, a
// redelivered notification, a purchase outside every offer).
func RedeemWinbackOfferInTx(tx server.PgTx, ctx context.Context, networkId server.Id, store string, offerId *server.Id, at time.Time) (redeemed *server.Id) {
	result, err := tx.Query(
		ctx,
		`
			UPDATE network_winback_offer
			SET redeemed_at = $3
			WHERE offer_id = (
				SELECT offer_id
				FROM network_winback_offer
				WHERE
					network_id = $1 AND
					store = $2 AND
					redeemed_at IS NULL AND
					issued_at <= $4 AND
					$3 < expires_at AND
					($5::uuid IS NULL OR offer_id = $5)
				ORDER BY issued_at DESC
				LIMIT 1
				FOR UPDATE
			)
			RETURNING offer_id
		`,
		networkId,
		store,
		at,
		at.Add(WinbackOfferIssueSkew),
		offerId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var id server.Id
			server.Raise(result.Scan(&id))
			redeemed = &id
		}
	})
	return
}

// LatestWinbackOfferForStoreInTx is the network's newest offer on `store` in
// any state, or nil: a purchase under the offer's handle with none is one the
// program never offered.
func LatestWinbackOfferForStoreInTx(tx server.PgTx, ctx context.Context, networkId server.Id, store string) (offer *WinbackOffer) {
	result, err := tx.Query(
		ctx,
		`
			SELECT `+winbackOfferColumns+`
			FROM network_winback_offer
			WHERE network_id = $1 AND store = $2
			ORDER BY issued_at DESC
			LIMIT 1
		`,
		networkId,
		store,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			offer = scanWinbackOffer(result)
		}
	})
	return
}

// ----- the facts the offer decision reads -----

// The markets whose renewal revenue is the amount the customer was charged:
// a Stripe trial invoice records zero, and the crypto, Gamma, x402 and manual
// markets only ever record a payment. The App Store and Play are not here: an
// App Store trial records a paid marker and a Play window records the plan's
// price whatever the store charged (NetworkHasPaidSupporterPeriod).
var winbackChargedRevenueMarkets = []string{
	SubscriptionMarketStripe,
	SubscriptionMarketSolana,
	SubscriptionMarketMonero,
	SubscriptionMarketZcash,
	SubscriptionMarketBittensor,
	SubscriptionMarketBitcoin,
	SubscriptionMarketEthereum,
	SubscriptionMarketGamma,
	SubscriptionMarketX402,
	SubscriptionMarketManual,
}

// The markets and actions of the operator events a refund, a dispute or a
// store revocation writes (payment_reconciliation_event), read through the
// (store, action, event_time) index. Gamma has no reconciliation leg and
// cannot be refunded on chain; an event an operator writes for it counts like
// any other.
var winbackRefundEventStores = []string{
	SubscriptionMarketStripe,
	SubscriptionMarketApple,
	SubscriptionMarketGoogle,
	SubscriptionMarketSolana,
	SubscriptionMarketMonero,
	SubscriptionMarketZcash,
	SubscriptionMarketBittensor,
	SubscriptionMarketBitcoin,
	SubscriptionMarketEthereum,
	SubscriptionMarketGamma,
	SubscriptionMarketX402,
	SubscriptionMarketManual,
}

var winbackRefundEventActions = []string{
	PaymentReconcileActionRefunded,
	PaymentReconcileActionDisputed,
	PaymentReconcileActionRevoked,
}

// NetworkHasPaidSupporterPeriod reports whether the network paid for at least
// one Pro period and nothing was given back: no Stripe refund or dispute and no
// refund, dispute or store revocation on any market. A period is paid when:
//
//   - Stripe, crypto, Gamma, x402, manual: its recorded revenue is above zero;
//   - App Store: its transaction's price in the ledger is above zero (a free
//     trial or free offer is priced zero, and a sandbox purchase records no
//     revenue);
//   - Play: its window outlasts a free phase and the store's day of slack
//     (reengagement.PlayTrialWindowDays), since a Play window records the
//     plan's price whether or not the store charged it.
//
// Any refund at all makes the network unpaid here: an offer is a price, and
// it never goes to anyone whose money went back.
func NetworkHasPaidSupporterPeriod(ctx context.Context, networkId server.Id) (paid bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					EXISTS (
						SELECT 1
						FROM subscription_renewal renewal
						WHERE
							renewal.network_id = $1 AND
							renewal.subscription_type = 'supporter' AND
							0 < renewal.net_revenue_nano_cents AND (
								renewal.market = ANY($2::text[]) OR
								(
									renewal.market = $3 AND
									renewal.start_time + make_interval(days => $4) < renewal.end_time
								) OR
								(
									renewal.market = $5 AND
									EXISTS (
										SELECT 1
										FROM apple_subscription_transaction ledger
										WHERE ledger.transaction_id = renewal.transaction_id AND 0 < ledger.price_milliunits
									)
								)
							)
					) AND
					NOT EXISTS (
						SELECT 1 FROM stripe_refund WHERE stripe_refund.network_id = $1
					) AND
					NOT EXISTS (
						SELECT 1
						FROM payment_reconciliation_event event
						WHERE
							event.store = ANY($6::text[]) AND
							event.action = ANY($7::text[]) AND
							event.network_id = $1 AND
							NOT event.dry_run
					)
			`,
			networkId,
			winbackChargedRevenueMarkets,
			SubscriptionMarketGoogle,
			reengagement.PlayTrialWindowDays,
			SubscriptionMarketApple,
			winbackRefundEventStores,
			winbackRefundEventActions,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&paid))
			}
		})
	})
	return
}

// NetworkLastAppleTransactionId is the transaction of the network's latest
// App Store supporter window, "" when it has none: the App Store Server API
// finds the customer's subscription statuses from any of its transactions.
func NetworkLastAppleTransactionId(ctx context.Context, networkId server.Id) (transactionId string) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT transaction_id
				FROM subscription_renewal
				WHERE
					network_id = $1 AND
					subscription_type = 'supporter' AND
					market = $2 AND
					COALESCE(transaction_id, '') <> ''
				ORDER BY end_time DESC
				LIMIT 1
			`,
			networkId,
			SubscriptionMarketApple,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&transactionId))
			}
		})
	})
	return
}
