package model

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// The crypto payment rail's tables (mmm/ZCASH-CONTRACT.md §3.1): an
// invoice per purchase (crypto_payment_intent) with its re-quote history
// (crypto_payment_quote), every incoming output the scanner saw
// (crypto_payment_receipt), the pool of pre-derived receive addresses
// (crypto_payment_address) and each coin's scan cursor (crypto_scan_cursor).
//
// The controller (controller/crypto_pay_*.go) owns the policy: what a quote
// is, when a payment is enough, and the one credit transaction. This file only
// reads and writes rows, with an InTx/InConn form wherever a caller already
// holds a transaction or a connection.

// The coins, by the name every API and table uses. "currency" already means
// the fiat currency ("USD") elsewhere, so the coin is an asset.
const (
	CryptoAssetUsdcSolana = "usdc_solana"
	CryptoAssetZec        = "zec"
	CryptoAssetXmr        = "xmr"
)

// Where a paid invoice delivers: the signed-in caller's network, a network
// named without signing in, or a balance code shown on the page (and emailed
// when an address was given).
const (
	CryptoDeliveryAccount = "account"
	CryptoDeliveryNetwork = "network"
	CryptoDeliveryCode    = "code"
)

// A receipt's state. seen: in the mempool (not counted while the wallet
// flags it as a double spend, review_reason double_spend); confirmed: in a
// block, not yet credited; credited: part of a credited payment; review:
// needs an operator (review_reason says why); orphaned: dropped from the
// chain before it was credited, so it no longer counts (it comes back if the
// chain shows it again); ignored: never counted (time-locked, or a
// transparent Zcash output).
const (
	CryptoReceiptStateSeen      = "seen"
	CryptoReceiptStateConfirmed = "confirmed"
	CryptoReceiptStateCredited  = "credited"
	CryptoReceiptStateReview    = "review"
	CryptoReceiptStateOrphaned  = "orphaned"
	CryptoReceiptStateIgnored   = "ignored"
)

// Why a receipt is in review or ignored.
const (
	// short of the quote when it expired
	CryptoReviewReasonUnderpaid = "underpaid"
	// arrived after the quote expired and is worth less than the price at its
	// first-seen rate
	CryptoReviewReasonLateUnderpaid = "late_underpaid"
	// a payment to an invoice that was already paid
	CryptoReviewReasonExtra = "extra"
	// paid to an address no invoice was given (or to the wallet's primary
	// address)
	CryptoReviewReasonUnknownAddress = "unknown_address"
	// a credited payment that dropped out of the chain; its entitlement was
	// ended
	CryptoReviewReasonOrphaned = "orphaned"
	// paid to an invoice an operator closed (bringyourctl crypto void)
	CryptoReviewReasonVoided = "voided"
	// ignored: carries an unlock time
	CryptoReviewReasonLocked = "locked"
	// seen: the wallet flags the mempool transaction as a double spend, so it
	// does not count until it is mined
	CryptoReviewReasonDoubleSpend = "double_spend"
	// the credit kept failing (CryptoPaymentCreditFailureLimit times in a
	// row): an operator finds out why and credits by hand
	CryptoReviewReasonCreditFailed = "credit_failed"
	// ignored: a transparent Zcash output (decision 4: shielded only)
	CryptoReviewReasonTransparent = "transparent"
)

// The most status tokens one invoice keeps: the first plus the ones handed to
// later callers that reused the open invoice.
const CryptoPaymentIntentMaxStatusTokens = 8

var ErrCryptoAddressPoolEmpty = errors.New("no free receive address in the pool")

// CryptoPaymentIntent is one invoice row.
type CryptoPaymentIntent struct {
	IntentId               server.Id
	StatusTokenHash        []byte
	ExtraStatusTokenHashes [][]byte
	Asset                  string
	Delivery               string
	// nil for code delivery, and after the retention unlink
	NetworkId *server.Id
	// a plan (yearly, yearly_onboarding) or a data item (data_1tib, data_10tib)
	Item         string
	Tier         string
	OfferApplied bool
	// what the buyer pays, and the item's regular price (the offer's full
	// price for yearly_onboarding)
	PriceUsdNanoCents        NanoCents
	RegularPriceUsdNanoCents NanoCents
	// code delivery only; cleared at the unlink
	Email        string
	Address      string
	AddressIndex *int64
	// usdc_solana: the linked solana_payment_intent's reference
	SolanaReference *string
	AmountAtomic    int64
	RateUsd         float64
	// the rate sources' answers behind the quote, json
	RateSourcesJson string
	QuotedAt        time.Time
	ExpiresAt       time.Time
	CreateTime      time.Time
	CreditedAt      *time.Time
	// the credit's USD value: the price for an on-time payment, the receipts'
	// first-seen value capped at the price for a late one
	CreditedUsdNanoCents *NanoCents
	// the transfer balance the credit granted (plan and account/network data)
	BalanceId *server.Id
	// the balance code the credit created (code delivery)
	BalanceCodeId *server.Id
	VoidedAt      *time.Time
	// the operator refund (one-shot)
	RefundedAt *time.Time
	// the credits of the invoice that failed in a row
	CreditFailureCount int
	UnlinkedAt         *time.Time
}

// MatchesStatusToken reports whether the hash of a presented status token is
// one this invoice handed out. Every stored hash is compared in constant time,
// so the answer's timing says nothing about which one matched or how much.
func (self *CryptoPaymentIntent) MatchesStatusToken(tokenHash []byte) bool {
	match := 0
	for _, stored := range append([][]byte{self.StatusTokenHash}, self.ExtraStatusTokenHashes...) {
		match |= subtle.ConstantTimeCompare(stored, tokenHash)
	}
	return match == 1
}

const cryptoPaymentIntentColumns = `
	intent_id,
	status_token_hash,
	extra_status_token_hashes,
	asset,
	delivery,
	network_id,
	item,
	tier,
	offer_applied,
	price_usd_nano_cents,
	regular_price_usd_nano_cents,
	email,
	address,
	address_index,
	solana_reference,
	amount_atomic,
	rate_usd,
	rate_sources::text,
	quoted_at,
	expires_at,
	create_time,
	credited_at,
	credited_usd_nano_cents,
	balance_id,
	balance_code_id,
	voided_at,
	refunded_at,
	credit_failure_count,
	unlinked_at
`

func scanCryptoPaymentIntent(result pgx.Rows) *CryptoPaymentIntent {
	intent := &CryptoPaymentIntent{}
	var tier *string
	var email *string
	var rateSources *string
	server.Raise(result.Scan(
		&intent.IntentId,
		&intent.StatusTokenHash,
		&intent.ExtraStatusTokenHashes,
		&intent.Asset,
		&intent.Delivery,
		&intent.NetworkId,
		&intent.Item,
		&tier,
		&intent.OfferApplied,
		&intent.PriceUsdNanoCents,
		&intent.RegularPriceUsdNanoCents,
		&email,
		&intent.Address,
		&intent.AddressIndex,
		&intent.SolanaReference,
		&intent.AmountAtomic,
		&intent.RateUsd,
		&rateSources,
		&intent.QuotedAt,
		&intent.ExpiresAt,
		&intent.CreateTime,
		&intent.CreditedAt,
		&intent.CreditedUsdNanoCents,
		&intent.BalanceId,
		&intent.BalanceCodeId,
		&intent.VoidedAt,
		&intent.RefundedAt,
		&intent.CreditFailureCount,
		&intent.UnlinkedAt,
	))
	if tier != nil {
		intent.Tier = *tier
	}
	if email != nil {
		intent.Email = *email
	}
	if rateSources != nil {
		intent.RateSourcesJson = *rateSources
	}
	return intent
}

func nullIfEmptyString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// CreateCryptoPaymentIntentInTx inserts the invoice and its first quote. For
// xmr and zec the caller has already allocated the address in the same
// transaction (AllocateCryptoPaymentAddressInTx), so the address and the
// invoice commit together or not at all.
func CreateCryptoPaymentIntentInTx(tx server.PgTx, ctx context.Context, intent *CryptoPaymentIntent) {
	if intent.CreateTime.IsZero() {
		intent.CreateTime = server.NowUtc()
	}
	if intent.ExtraStatusTokenHashes == nil {
		intent.ExtraStatusTokenHashes = [][]byte{}
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO crypto_payment_intent (
				intent_id,
				status_token_hash,
				extra_status_token_hashes,
				asset,
				delivery,
				network_id,
				item,
				tier,
				offer_applied,
				price_usd_nano_cents,
				regular_price_usd_nano_cents,
				email,
				address,
				address_index,
				solana_reference,
				amount_atomic,
				rate_usd,
				rate_sources,
				quoted_at,
				expires_at,
				create_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18::jsonb, $19, $20, $21)
		`,
		intent.IntentId,
		intent.StatusTokenHash,
		intent.ExtraStatusTokenHashes,
		intent.Asset,
		intent.Delivery,
		intent.NetworkId,
		intent.Item,
		nullIfEmptyString(intent.Tier),
		intent.OfferApplied,
		intent.PriceUsdNanoCents,
		intent.RegularPriceUsdNanoCents,
		nullIfEmptyString(intent.Email),
		intent.Address,
		intent.AddressIndex,
		intent.SolanaReference,
		intent.AmountAtomic,
		intent.RateUsd,
		nullIfEmptyString(intent.RateSourcesJson),
		intent.QuotedAt,
		intent.ExpiresAt,
		intent.CreateTime,
	))
	addCryptoPaymentQuoteInTx(tx, ctx, intent.IntentId, intent.QuotedAt, intent.AmountAtomic, intent.RateUsd, intent.RateSourcesJson, intent.ExpiresAt)
}

func addCryptoPaymentQuoteInTx(
	tx server.PgTx,
	ctx context.Context,
	intentId server.Id,
	quotedAt time.Time,
	amountAtomic int64,
	rateUsd float64,
	rateSourcesJson string,
	expiresAt time.Time,
) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO crypto_payment_quote (intent_id, quoted_at, amount_atomic, rate_usd, rate_sources, expires_at)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6)
			ON CONFLICT (intent_id, quoted_at) DO NOTHING
		`,
		intentId,
		quotedAt,
		amountAtomic,
		rateUsd,
		nullIfEmptyString(rateSourcesJson),
		expiresAt,
	))
}

// CreateCryptoPaymentIntentWithAddress allocates a pool address (when
// allocateAddress) and inserts the invoice in one transaction. An empty pool
// is ErrCryptoAddressPoolEmpty and writes nothing.
func CreateCryptoPaymentIntentWithAddress(ctx context.Context, intent *CryptoPaymentIntent, allocateAddress bool) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = nil
		if allocateAddress {
			address, addressIndex, ok := AllocateCryptoPaymentAddressInTx(tx, ctx, intent.Asset, intent.IntentId, server.NowUtc())
			if !ok {
				returnErr = ErrCryptoAddressPoolEmpty
				return
			}
			intent.Address = address
			intent.AddressIndex = &addressIndex
		}
		CreateCryptoPaymentIntentInTx(tx, ctx, intent)
	}, server.TxReadCommitted)
	return
}

// GetCryptoPaymentIntent reads one invoice, or nil.
func GetCryptoPaymentIntent(ctx context.Context, intentId server.Id) *CryptoPaymentIntent {
	return GetCryptoPaymentIntentInConn(nil, ctx, intentId)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func GetCryptoPaymentIntentInConn(connOwner server.PgConn, ctx context.Context, intentId server.Id) (intent *CryptoPaymentIntent) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		intent = getCryptoPaymentIntent(ctx, conn, intentId)
	})
	return
}

// GetCryptoPaymentIntentInTx reads one invoice in the caller's transaction.
func GetCryptoPaymentIntentInTx(tx server.PgTx, ctx context.Context, intentId server.Id) *CryptoPaymentIntent {
	return getCryptoPaymentIntent(ctx, tx, intentId)
}

func getCryptoPaymentIntent(ctx context.Context, query server.PgCanQuery, intentId server.Id) (intent *CryptoPaymentIntent) {
	result, err := query.Query(
		ctx,
		`SELECT `+cryptoPaymentIntentColumns+` FROM crypto_payment_intent WHERE intent_id = $1`,
		intentId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			intent = scanCryptoPaymentIntent(result)
		}
	})
	return
}

// GetCryptoPaymentIntentBySolanaReference is the invoice linked to a Solana
// Pay intent, or nil when the Solana intent was made by the old endpoints.
func GetCryptoPaymentIntentBySolanaReference(ctx context.Context, reference string) *CryptoPaymentIntent {
	return GetCryptoPaymentIntentBySolanaReferenceInConn(nil, ctx, reference)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func GetCryptoPaymentIntentBySolanaReferenceInConn(connOwner server.PgConn, ctx context.Context, reference string) (intent *CryptoPaymentIntent) {
	if reference == "" {
		return nil
	}
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+cryptoPaymentIntentColumns+` FROM crypto_payment_intent WHERE solana_reference = $1`,
			reference,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				intent = scanCryptoPaymentIntent(result)
			}
		})
	})
	return
}

// GetCryptoPaymentIntentsByAddressesInConn maps each given receive address of
// a coin to its invoice. Addresses no invoice was given are absent from the
// map. Reuse the caller's PostgreSQL session; nil selects the outer
// acquisition boundary.
func GetCryptoPaymentIntentsByAddressesInConn(
	connOwner server.PgConn,
	ctx context.Context,
	asset string,
	addresses []string,
) map[string]*CryptoPaymentIntent {
	intents := map[string]*CryptoPaymentIntent{}
	if len(addresses) == 0 {
		return intents
	}
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+cryptoPaymentIntentColumns+`
				FROM crypto_payment_intent
				WHERE asset = $1 AND address = ANY($2) AND solana_reference IS NULL
			`,
			asset,
			addresses,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				intent := scanCryptoPaymentIntent(result)
				intents[intent.Address] = intent
			}
		})
	})
	return intents
}

// GetCryptoPaymentIntentsByAddressIndexesInConn maps each given address index
// of a coin (a Monero subaddress minor index, a Zcash diversifier index) to
// the invoice given that address. Indexes no invoice was given are absent.
// The scanner matches receipts this way: zecd reports the single receiver a
// payment reached, not the unified address that was handed out, so only the
// index identifies the invoice.
func GetCryptoPaymentIntentsByAddressIndexesInConn(
	connOwner server.PgConn,
	ctx context.Context,
	asset string,
	addressIndexes []int64,
) map[int64]*CryptoPaymentIntent {
	intents := map[int64]*CryptoPaymentIntent{}
	if len(addressIndexes) == 0 {
		return intents
	}
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+cryptoPaymentIntentColumns+`
				FROM crypto_payment_intent
				WHERE asset = $1 AND address_index = ANY($2)
			`,
			asset,
			addressIndexes,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				intent := scanCryptoPaymentIntent(result)
				if intent.AddressIndex != nil {
					intents[*intent.AddressIndex] = intent
				}
			}
		})
	})
	return intents
}

// GetOpenCryptoPaymentIntentForReuse is the network's newest invoice for the
// coin, item and delivery that is not credited, not voided and not expired,
// and none of whose receipts waits for an operator; nil when there is none. A
// second request returns this invoice (same address, same quote) instead of
// drawing another address: one open invoice per network, coin, item and
// delivery (decision 13, mmm/ZCASH-CONTRACT.md §3.13.5). The delivery is part of the
// key, so the buy-data page (delivery network, no sign-in) never receives a
// token for the account's own signed-in invoice, and the reverse.
func GetOpenCryptoPaymentIntentForReuse(
	ctx context.Context,
	networkId server.Id,
	asset string,
	item string,
	delivery string,
	now time.Time,
) (intent *CryptoPaymentIntent) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+cryptoPaymentIntentColumns+`
				FROM crypto_payment_intent
				WHERE network_id = $1
				  AND asset = $2
				  AND item = $3
				  AND delivery = $5
				  AND credited_at IS NULL
				  AND $4 < expires_at
				  AND voided_at IS NULL
				  AND NOT EXISTS (
					SELECT 1 FROM crypto_payment_receipt receipt
					WHERE receipt.intent_id = crypto_payment_intent.intent_id
					  AND receipt.state = 'review'
				  )
				ORDER BY create_time DESC
				LIMIT 1
			`,
			networkId,
			asset,
			item,
			now,
			delivery,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				intent = scanCryptoPaymentIntent(result)
			}
		})
	})
	return
}

// AddCryptoPaymentIntentStatusToken keeps one more status token for an open
// invoice handed to a new caller, so every caller that holds one can poll. The
// oldest extra tokens are dropped past CryptoPaymentIntentMaxStatusTokens.
func AddCryptoPaymentIntentStatusToken(ctx context.Context, intentId server.Id, tokenHash []byte) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = nil
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE crypto_payment_intent
				SET extra_status_token_hashes = (
					array_append(extra_status_token_hashes, $2::bytea)
				)[greatest(1, cardinality(extra_status_token_hashes) + 2 - $3):]
				WHERE intent_id = $1
			`,
			intentId,
			tokenHash,
			CryptoPaymentIntentMaxStatusTokens-1,
		))
		if tag.RowsAffected() == 0 {
			returnErr = errors.New("crypto payment intent not found")
		}
	}, server.TxReadCommitted)
	return
}

// CountOpenCryptoPaymentIntents is the number of the coin's invoices that are
// neither credited nor expired, of one delivery ("" counts every delivery).
func CountOpenCryptoPaymentIntents(ctx context.Context, asset string, delivery string, now time.Time) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT count(*)
				FROM crypto_payment_intent
				WHERE asset = $1 AND credited_at IS NULL AND $2 < expires_at
				  AND ($3 = '' OR delivery = $3)
			`,
			asset,
			now,
			delivery,
		).Scan(&count))
	})
	return
}

// CryptoPaymentQuote is one quote of an invoice.
type CryptoPaymentQuote struct {
	QuotedAt        time.Time
	AmountAtomic    int64
	RateUsd         float64
	RateSourcesJson string
	ExpiresAt       time.Time
}

// RequoteCryptoPaymentIntent gives an open invoice a new amount, rate and
// expiry for the same address and keeps the quote in the history. Refused
// (requoted = false) once the invoice is credited or voided, or once anything
// was received for it: a quote must not change under a payment.
func RequoteCryptoPaymentIntent(ctx context.Context, intentId server.Id, quote *CryptoPaymentQuote) (requoted bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		requoted = false
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE crypto_payment_intent
				SET amount_atomic = $2,
					rate_usd = $3,
					rate_sources = $4::jsonb,
					quoted_at = $5,
					expires_at = $6
				WHERE intent_id = $1
				  AND credited_at IS NULL
				  AND voided_at IS NULL
				  AND NOT EXISTS (
					SELECT 1 FROM crypto_payment_receipt receipt
					WHERE receipt.intent_id = $1
					  AND receipt.state NOT IN ('ignored', 'orphaned')
				  )
			`,
			intentId,
			quote.AmountAtomic,
			quote.RateUsd,
			nullIfEmptyString(quote.RateSourcesJson),
			quote.QuotedAt,
			quote.ExpiresAt,
		))
		if tag.RowsAffected() == 0 {
			return
		}
		addCryptoPaymentQuoteInTx(tx, ctx, intentId, quote.QuotedAt, quote.AmountAtomic, quote.RateUsd, quote.RateSourcesJson, quote.ExpiresAt)
		requoted = true
	}, server.TxReadCommitted)
	return
}

// ExtendSolanaCryptoPaymentIntent moves a USDC invoice's expiry, with its
// Solana intent's and its unique-amount reservation's, while neither is
// consumed. A USDC quote is already in dollars, so a "re-quote" only gives the
// buyer more time. extended = false when the invoice is credited or voided.
//
// Both rows are locked before either is written, invoice first and Solana
// intent second: the order the credit takes them in, so the two never wait on
// each other in a cycle.
func ExtendSolanaCryptoPaymentIntent(ctx context.Context, intentId server.Id, reference string, quotedAt time.Time, expiresAt time.Time) (extended bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		extended = false
		open := false
		result, err := tx.Query(
			ctx,
			`
				SELECT 1 FROM crypto_payment_intent
				WHERE intent_id = $1 AND solana_reference = $2 AND credited_at IS NULL AND voided_at IS NULL
				FOR UPDATE
			`,
			intentId,
			reference,
		)
		server.WithPgResult(result, err, func() {
			open = result.Next()
		})
		if !open {
			return
		}
		open = false
		result, err = tx.Query(
			ctx,
			`
				SELECT 1 FROM solana_payment_intent
				WHERE payment_reference = $1 AND tx_signature IS NULL
				FOR UPDATE
			`,
			reference,
		)
		server.WithPgResult(result, err, func() {
			open = result.Next()
		})
		if !open {
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE solana_payment_intent SET expires_at = $2 WHERE payment_reference = $1`,
			reference,
			expiresAt,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE crypto_payment_intent SET quoted_at = $2, expires_at = $3 WHERE intent_id = $1`,
			intentId,
			quotedAt,
			expiresAt,
		))
		// the amount stays reserved for the hold past the new expiry, while this
		// intent still holds it
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE solana_payment_amount_reservation
				SET reserved_until = greatest(reserved_until, $2)
				WHERE payment_reference = $1
			`,
			reference,
			expiresAt.Add(SolanaUniqueAmountHold),
		))
		var amountAtomic int64
		var rateUsd float64
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT amount_atomic, rate_usd FROM crypto_payment_intent WHERE intent_id = $1`,
			intentId,
		).Scan(&amountAtomic, &rateUsd))
		addCryptoPaymentQuoteInTx(tx, ctx, intentId, quotedAt, amountAtomic, rateUsd, "", expiresAt)
		extended = true
	}, server.TxReadCommitted)
	return
}

// MarkCryptoPaymentIntentCreditedInTx is the invoice one-shot. credited =
// false means another caller already credited it, an operator voided it, or
// it does not exist: the caller must NOT credit. Under READ COMMITTED a second
// UPDATE blocks on the row lock, re-evaluates `credited_at IS NULL` after the
// first commits, and affects zero rows -- so a scanner run racing a reconcile
// credit of the same invoice credits exactly once, and a void that commits
// first wins over a credit (mmm/ZCASH-CONTRACT.md §3.13.12).
func MarkCryptoPaymentIntentCreditedInTx(
	tx server.PgTx,
	ctx context.Context,
	intentId server.Id,
	creditedAt time.Time,
	creditedUsdNanoCents NanoCents,
) (credited bool) {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE crypto_payment_intent
			SET credited_at = $2, credited_usd_nano_cents = $3
			WHERE intent_id = $1 AND credited_at IS NULL AND voided_at IS NULL
		`,
		intentId,
		creditedAt,
		creditedUsdNanoCents,
	))
	return tag.RowsAffected() == 1
}

// SetCryptoPaymentIntentGrantInTx records what the credit granted: the
// transfer balance and/or the balance code.
func SetCryptoPaymentIntentGrantInTx(
	tx server.PgTx,
	ctx context.Context,
	intentId server.Id,
	balanceId *server.Id,
	balanceCodeId *server.Id,
) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE crypto_payment_intent
			SET balance_id = $2, balance_code_id = $3
			WHERE intent_id = $1
		`,
		intentId,
		balanceId,
		balanceCodeId,
	))
}

// UncreditCryptoPaymentIntentInTx takes back an invoice's credit after what
// it granted was ended because the paying transaction left the chain: the
// invoice is open again (not credited), with its receipts held for review, so
// `bringyourctl crypto credit` can grant it again through the one credit. A
// code's balance code stops naming the invoice, so a new credit makes a new
// code. uncredited = false when it was not credited.
func UncreditCryptoPaymentIntentInTx(tx server.PgTx, ctx context.Context, intentId server.Id) (uncredited bool) {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE crypto_payment_intent
			SET credited_at = NULL,
				credited_usd_nano_cents = NULL,
				balance_id = NULL,
				balance_code_id = NULL,
				credit_failure_count = 0
			WHERE intent_id = $1 AND credited_at IS NOT NULL
		`,
		intentId,
	))
	uncredited = tag.RowsAffected() == 1
	if uncredited {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE transfer_balance_code
				SET purchase_event_id = $2, purchase_email = ''
				WHERE purchase_event_id = $1
			`,
			CryptoPaymentPurchaseEventId(intentId),
			"crypto/ended/"+server.NewId().String(),
		))
	}
	return
}

// MarkCryptoPaymentIntentRefundedInTx is the operator refund's one-shot: only
// a credited invoice that is still linked to its network and was not refunded
// yet is marked. refunded = false otherwise.
func MarkCryptoPaymentIntentRefundedInTx(tx server.PgTx, ctx context.Context, intentId server.Id, now time.Time) (refunded bool) {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE crypto_payment_intent
			SET refunded_at = $2
			WHERE intent_id = $1
			  AND refunded_at IS NULL
			  AND credited_at IS NOT NULL
			  AND unlinked_at IS NULL
		`,
		intentId,
		now,
	))
	return tag.RowsAffected() == 1
}

// The credits of one invoice that may fail in a row before its receipts go to
// review (credit_failed).
const CryptoPaymentCreditFailureLimit = 5

// AddCryptoPaymentIntentCreditFailure counts one more failed credit of an
// invoice and returns the count.
func AddCryptoPaymentIntentCreditFailure(ctx context.Context, intentId server.Id) (count int) {
	server.Tx(ctx, func(tx server.PgTx) {
		count = 0
		err := tx.QueryRow(
			ctx,
			`
				UPDATE crypto_payment_intent
				SET credit_failure_count = credit_failure_count + 1
				WHERE intent_id = $1 AND credited_at IS NULL
				RETURNING credit_failure_count
			`,
			intentId,
		).Scan(&count)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
	}, server.TxReadCommitted)
	return
}

// VoidCryptoPaymentIntent closes an open invoice by hand. A payment that
// still arrives for it goes to review. voided = false when it is credited,
// already void, or missing.
func VoidCryptoPaymentIntent(ctx context.Context, intentId server.Id, now time.Time) (voided bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE crypto_payment_intent
				SET voided_at = $2
				WHERE intent_id = $1 AND credited_at IS NULL AND voided_at IS NULL
			`,
			intentId,
			now,
		))
		voided = tag.RowsAffected() == 1
	}, server.TxReadCommitted)
	return
}

// ListCryptoPaymentIntents lists invoices for the operator tools, newest
// first. asset "" lists every coin.
func ListCryptoPaymentIntents(ctx context.Context, asset string, limit int) (intents []*CryptoPaymentIntent) {
	intents = []*CryptoPaymentIntent{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+cryptoPaymentIntentColumns+`
				FROM crypto_payment_intent
				WHERE ($1 = '' OR asset = $1)
				ORDER BY create_time DESC
				LIMIT $2
			`,
			asset,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				intents = append(intents, scanCryptoPaymentIntent(result))
			}
		})
	})
	return
}

// ----- receipts -----

// CryptoPaymentReceipt is one incoming output, keyed by the coin, the
// transaction and the output within it.
type CryptoPaymentReceipt struct {
	Asset     string
	TxId      string
	OutputKey string
	// nil: paid to an address no invoice was given
	IntentId *server.Id
	// the address as the wallet reports it, and its index in the wallet (the
	// Monero subaddress minor index, the Zcash diversifier index)
	Address      string
	AddressIndex *int64
	// zec: the pool the output landed in (orchard, ironwood, sapling,
	// transparent), as last reported
	Pool         string
	AmountAtomic int64
	// nil while in the mempool
	BlockHeight   *int64
	Confirmations int
	// when it was first seen: the daemon's mempool or block time, not before
	// the invoice's quote and not after the scan (mmm/ZCASH-CONTRACT.md §3.13.4)
	FirstSeenAt time.Time
	// the wallet's height when it was first seen
	FirstSeenHeight  *int64
	FirstSeenRateUsd *float64
	ConfirmedAt      *time.Time
	FinalAt          *time.Time
	// this receipt's share of the credited USD value
	UsdValueNanoCents *NanoCents
	State             string
	ReviewReason      string
	UpdateTime        time.Time
}

// Key is the receipt's identity.
func (self *CryptoPaymentReceipt) Key() CryptoPaymentReceiptKey {
	return CryptoPaymentReceiptKey{Asset: self.Asset, TxId: self.TxId, OutputKey: self.OutputKey}
}

type CryptoPaymentReceiptKey struct {
	Asset     string
	TxId      string
	OutputKey string
}

const cryptoPaymentReceiptColumns = `
	asset,
	tx_id,
	output_key,
	intent_id,
	address,
	address_index,
	pool,
	amount_atomic,
	block_height,
	confirmations,
	first_seen_at,
	first_seen_height,
	first_seen_rate_usd,
	confirmed_at,
	final_at,
	usd_value_nano_cents,
	state,
	review_reason,
	update_time
`

func scanCryptoPaymentReceipt(result pgx.Rows) *CryptoPaymentReceipt {
	receipt := &CryptoPaymentReceipt{}
	var address *string
	var pool *string
	var reviewReason *string
	server.Raise(result.Scan(
		&receipt.Asset,
		&receipt.TxId,
		&receipt.OutputKey,
		&receipt.IntentId,
		&address,
		&receipt.AddressIndex,
		&pool,
		&receipt.AmountAtomic,
		&receipt.BlockHeight,
		&receipt.Confirmations,
		&receipt.FirstSeenAt,
		&receipt.FirstSeenHeight,
		&receipt.FirstSeenRateUsd,
		&receipt.ConfirmedAt,
		&receipt.FinalAt,
		&receipt.UsdValueNanoCents,
		&receipt.State,
		&reviewReason,
		&receipt.UpdateTime,
	))
	if address != nil {
		receipt.Address = *address
	}
	if pool != nil {
		receipt.Pool = *pool
	}
	if reviewReason != nil {
		receipt.ReviewReason = *reviewReason
	}
	return receipt
}

// UpsertCryptoPaymentReceipt records what the wallet reports about one output,
// idempotently on (asset, tx_id, output_key): a scan that sees it again never
// adds a second receipt. A new receipt takes the state and reason the scanner
// decided. A known one only takes the new observation (height,
// confirmations, its amount until it is credited, a first-seen rate or
// height it lacked) and moves between seen and confirmed: a mempool receipt
// the wallet flags as a double spend is seen (double_spend) and becomes
// confirmed when it is mined, and a receipt dropped before its credit
// (orphaned) comes back when the chain shows it again. An ignored receipt
// flagged as a double spend before this rule also becomes confirmed once
// mined (mmm/ZCASH-CONTRACT.md §3.13.3). Credited, review and the other ignored
// receipts keep their state: those are decisions, not observations. The
// first sighting's time stays. inserted reports a new receipt.
func UpsertCryptoPaymentReceiptInConn(connOwner server.PgConn, ctx context.Context, receipt *CryptoPaymentReceipt) (inserted bool, stored *CryptoPaymentReceipt) {
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		inserted, stored = UpsertCryptoPaymentReceiptInTx(tx, ctx, receipt)
	}, server.TxReadCommitted)
	return
}

func UpsertCryptoPaymentReceiptInTx(tx server.PgTx, ctx context.Context, receipt *CryptoPaymentReceipt) (inserted bool, stored *CryptoPaymentReceipt) {
	now := server.NowUtc()
	if receipt.FirstSeenAt.IsZero() {
		receipt.FirstSeenAt = now
	}
	var confirmedAt *time.Time
	if receipt.BlockHeight != nil {
		confirmedAt = &now
	}
	result, err := tx.Query(
		ctx,
		`
			INSERT INTO crypto_payment_receipt AS receipt (
				asset,
				tx_id,
				output_key,
				intent_id,
				address,
				address_index,
				pool,
				amount_atomic,
				block_height,
				confirmations,
				first_seen_at,
				first_seen_height,
				first_seen_rate_usd,
				confirmed_at,
				state,
				review_reason,
				update_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $17, $12, $13, $14, $15, $16)
			ON CONFLICT (asset, tx_id, output_key) DO UPDATE
			SET block_height = EXCLUDED.block_height,
				pool = COALESCE(EXCLUDED.pool, receipt.pool),
				amount_atomic = CASE
					WHEN receipt.state = 'credited' THEN receipt.amount_atomic
					ELSE EXCLUDED.amount_atomic
				END,
				confirmations = EXCLUDED.confirmations,
				confirmed_at = COALESCE(receipt.confirmed_at, EXCLUDED.confirmed_at),
				first_seen_height = COALESCE(receipt.first_seen_height, EXCLUDED.first_seen_height),
				first_seen_rate_usd = COALESCE(receipt.first_seen_rate_usd, EXCLUDED.first_seen_rate_usd),
				state = CASE
					WHEN receipt.state IN ('seen', 'confirmed', 'orphaned')
					  OR (receipt.state = 'ignored' AND receipt.review_reason = 'double_spend'
						  AND EXCLUDED.block_height IS NOT NULL) THEN
						CASE WHEN EXCLUDED.block_height IS NULL THEN 'seen' ELSE 'confirmed' END
					ELSE receipt.state
				END,
				review_reason = CASE
					WHEN receipt.state IN ('seen', 'confirmed', 'orphaned')
					  OR (receipt.state = 'ignored' AND receipt.review_reason = 'double_spend'
						  AND EXCLUDED.block_height IS NOT NULL) THEN
						CASE
							WHEN EXCLUDED.block_height IS NULL AND EXCLUDED.review_reason = 'double_spend' THEN 'double_spend'
							ELSE NULL
						END
					ELSE receipt.review_reason
				END,
				update_time = EXCLUDED.update_time
			RETURNING (xmax = 0), `+cryptoPaymentReceiptColumns,
		receipt.Asset,
		receipt.TxId,
		receipt.OutputKey,
		receipt.IntentId,
		nullIfEmptyString(receipt.Address),
		receipt.AddressIndex,
		nullIfEmptyString(receipt.Pool),
		receipt.AmountAtomic,
		receipt.BlockHeight,
		receipt.Confirmations,
		receipt.FirstSeenAt,
		receipt.FirstSeenRateUsd,
		confirmedAt,
		receipt.State,
		nullIfEmptyString(receipt.ReviewReason),
		now,
		receipt.FirstSeenHeight,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			stored = &CryptoPaymentReceipt{}
			var address *string
			var pool *string
			var reviewReason *string
			server.Raise(result.Scan(
				&inserted,
				&stored.Asset,
				&stored.TxId,
				&stored.OutputKey,
				&stored.IntentId,
				&address,
				&stored.AddressIndex,
				&pool,
				&stored.AmountAtomic,
				&stored.BlockHeight,
				&stored.Confirmations,
				&stored.FirstSeenAt,
				&stored.FirstSeenHeight,
				&stored.FirstSeenRateUsd,
				&stored.ConfirmedAt,
				&stored.FinalAt,
				&stored.UsdValueNanoCents,
				&stored.State,
				&reviewReason,
				&stored.UpdateTime,
			))
			if address != nil {
				stored.Address = *address
			}
			if pool != nil {
				stored.Pool = *pool
			}
			if reviewReason != nil {
				stored.ReviewReason = *reviewReason
			}
		}
	})
	return
}

// GetCryptoPaymentReceiptsForIntent lists an invoice's receipts, oldest first.
func GetCryptoPaymentReceiptsForIntent(ctx context.Context, intentId server.Id) []*CryptoPaymentReceipt {
	return GetCryptoPaymentReceiptsForIntentInConn(nil, ctx, intentId)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func GetCryptoPaymentReceiptsForIntentInConn(connOwner server.PgConn, ctx context.Context, intentId server.Id) (receipts []*CryptoPaymentReceipt) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		receipts = getCryptoPaymentReceiptsForIntent(ctx, conn, intentId)
	})
	return
}

// GetCryptoPaymentReceiptsForIntentInTx reads an invoice's receipts in the
// caller's transaction.
func GetCryptoPaymentReceiptsForIntentInTx(tx server.PgTx, ctx context.Context, intentId server.Id) []*CryptoPaymentReceipt {
	return getCryptoPaymentReceiptsForIntent(ctx, tx, intentId)
}

func getCryptoPaymentReceiptsForIntent(ctx context.Context, query server.PgCanQuery, intentId server.Id) []*CryptoPaymentReceipt {
	receipts := []*CryptoPaymentReceipt{}
	result, err := query.Query(
		ctx,
		`
			SELECT `+cryptoPaymentReceiptColumns+`
			FROM crypto_payment_receipt
			WHERE intent_id = $1
			ORDER BY first_seen_at, tx_id, output_key
		`,
		intentId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			receipts = append(receipts, scanCryptoPaymentReceipt(result))
		}
	})
	return receipts
}

// SetCryptoPaymentReceiptsStateInTx moves the given receipts to a state with
// its reason ("" clears it). Only receipts of the invoice move.
func SetCryptoPaymentReceiptsStateInTx(
	tx server.PgTx,
	ctx context.Context,
	intentId *server.Id,
	keys []CryptoPaymentReceiptKey,
	state string,
	reviewReason string,
) {
	for _, key := range keys {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE crypto_payment_receipt
				SET state = $5, review_reason = $6, update_time = $7
				WHERE asset = $1 AND tx_id = $2 AND output_key = $3
				  AND intent_id IS NOT DISTINCT FROM $4
			`,
			key.Asset,
			key.TxId,
			key.OutputKey,
			intentId,
			state,
			nullIfEmptyString(reviewReason),
			server.NowUtc(),
		))
	}
}

// SetCryptoPaymentReceiptsState is SetCryptoPaymentReceiptsStateInTx in its
// own transaction on the caller's connection.
func SetCryptoPaymentReceiptsStateInConn(
	connOwner server.PgConn,
	ctx context.Context,
	intentId *server.Id,
	keys []CryptoPaymentReceiptKey,
	state string,
	reviewReason string,
) {
	if len(keys) == 0 {
		return
	}
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		SetCryptoPaymentReceiptsStateInTx(tx, ctx, intentId, keys, state, reviewReason)
	}, server.TxReadCommitted)
}

// SetCryptoPaymentReceiptUsdValueInTx records a credited receipt's share of
// the credit's USD value.
func SetCryptoPaymentReceiptUsdValueInTx(tx server.PgTx, ctx context.Context, key CryptoPaymentReceiptKey, usdValueNanoCents NanoCents) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE crypto_payment_receipt
			SET usd_value_nano_cents = $4
			WHERE asset = $1 AND tx_id = $2 AND output_key = $3
		`,
		key.Asset,
		key.TxId,
		key.OutputKey,
		usdValueNanoCents,
	))
}

// SetCryptoPaymentReceiptFirstSeenRate fills a receipt's first-seen rate when
// it has none yet (the rate was unavailable when it was first seen).
func SetCryptoPaymentReceiptFirstSeenRateInConn(connOwner server.PgConn, ctx context.Context, key CryptoPaymentReceiptKey, rateUsd float64) {
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE crypto_payment_receipt
				SET first_seen_rate_usd = $4
				WHERE asset = $1 AND tx_id = $2 AND output_key = $3 AND first_seen_rate_usd IS NULL
			`,
			key.Asset,
			key.TxId,
			key.OutputKey,
			rateUsd,
		))
	}, server.TxReadCommitted)
}

// MarkCryptoPaymentReceiptsFinalInConn stamps final_at on the coin's receipts
// in blocks at least finalConfirmations deep. Returns how many it stamped.
func MarkCryptoPaymentReceiptsFinalInConn(
	connOwner server.PgConn,
	ctx context.Context,
	asset string,
	keys []CryptoPaymentReceiptKey,
	now time.Time,
) (marked int64) {
	if len(keys) == 0 {
		return 0
	}
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		marked = 0
		for _, key := range keys {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE crypto_payment_receipt
					SET final_at = $4
					WHERE asset = $1 AND tx_id = $2 AND output_key = $3
					  AND final_at IS NULL AND block_height IS NOT NULL
				`,
				asset,
				key.TxId,
				key.OutputKey,
				now,
			))
			marked += tag.RowsAffected()
		}
	}, server.TxReadCommitted)
	return
}

// ListCryptoPaymentReceiptsByStateInConn lists the coin's receipts in the
// given states, oldest first.
func ListCryptoPaymentReceiptsByStateInConn(
	connOwner server.PgConn,
	ctx context.Context,
	asset string,
	states []string,
	limit int,
) (receipts []*CryptoPaymentReceipt) {
	receipts = []*CryptoPaymentReceipt{}
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+cryptoPaymentReceiptColumns+`
				FROM crypto_payment_receipt
				WHERE ($1 = '' OR asset = $1) AND state = ANY($2)
				ORDER BY first_seen_at, tx_id, output_key
				LIMIT $3
			`,
			asset,
			states,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				receipts = append(receipts, scanCryptoPaymentReceipt(result))
			}
		})
	})
	return
}

// ListCryptoPaymentReceiptsToVerifyInConn lists the coin's credited receipts
// the reconciler re-checks against the wallet: those stamped final since
// finalSince, and those credited before staleBefore that never reached final
// depth (a credited transaction that left the chain stops being reported, so
// it would never be stamped final).
func ListCryptoPaymentReceiptsToVerifyInConn(
	connOwner server.PgConn,
	ctx context.Context,
	asset string,
	finalSince time.Time,
	staleBefore time.Time,
	limit int,
) (receipts []*CryptoPaymentReceipt) {
	receipts = []*CryptoPaymentReceipt{}
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+cryptoPaymentReceiptColumns+`
				FROM crypto_payment_receipt
				WHERE asset = $1
				  AND state = 'credited'
				  AND (
					$2 <= final_at
					OR (final_at IS NULL AND update_time < $3)
				  )
				ORDER BY first_seen_at, tx_id, output_key
				LIMIT $4
			`,
			asset,
			finalSince,
			staleBefore,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				receipts = append(receipts, scanCryptoPaymentReceipt(result))
			}
		})
	})
	return
}

// SumCryptoPaymentReceiptsInConn totals the coin's mined receipts in the
// height range [fromHeight, toHeight) that still count as on the chain (every
// state but orphaned), for the reconciler's ledger check against the wallet.
func SumCryptoPaymentReceiptsInConn(
	connOwner server.PgConn,
	ctx context.Context,
	asset string,
	fromHeight int64,
	toHeight int64,
) (totalAtomic int64, count int) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT COALESCE(sum(amount_atomic), 0)::bigint, count(*)
				FROM crypto_payment_receipt
				WHERE asset = $1
				  AND $2 <= block_height AND block_height < $3
				  AND state <> 'orphaned'
				  AND NOT (state = 'review' AND review_reason = 'orphaned')
			`,
			asset,
			fromHeight,
			toHeight,
		).Scan(&totalAtomic, &count))
	})
	return
}

// CountCryptoPaymentReceiptsByState counts the coin's receipts per state and
// review reason (the metrics).
func CountCryptoPaymentReceiptsByState(ctx context.Context, asset string) map[[2]string]int64 {
	counts := map[[2]string]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT state, COALESCE(review_reason, ''), count(*)
				FROM crypto_payment_receipt
				WHERE asset = $1
				GROUP BY state, COALESCE(review_reason, '')
			`,
			asset,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var state string
				var reason string
				var count int64
				server.Raise(result.Scan(&state, &reason, &count))
				counts[[2]string{state, reason}] = count
			}
		})
	})
	return counts
}

// ----- address pool -----

// AllocateCryptoPaymentAddressInTx takes the coin's lowest free pool address
// for an invoice. SKIP LOCKED: two invoices made at once never draw the same
// address, and neither waits on the other. ok = false when the pool is empty.
func AllocateCryptoPaymentAddressInTx(
	tx server.PgTx,
	ctx context.Context,
	asset string,
	intentId server.Id,
	now time.Time,
) (address string, addressIndex int64, ok bool) {
	err := tx.QueryRow(
		ctx,
		`
			UPDATE crypto_payment_address
			SET allocated_at = $3, intent_id = $2
			WHERE (asset, address_index) = (
				SELECT asset, address_index
				FROM crypto_payment_address
				WHERE asset = $1 AND allocated_at IS NULL
				ORDER BY address_index
				LIMIT 1
				FOR UPDATE SKIP LOCKED
			)
			RETURNING address, address_index
		`,
		asset,
		intentId,
		now,
	).Scan(&address, &addressIndex)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false
	}
	server.Raise(err)
	return address, addressIndex, true
}

// CryptoPaymentAddress is a pre-derived receive address.
type CryptoPaymentAddress struct {
	Asset        string
	AddressIndex int64
	Address      string
}

// AddCryptoPaymentAddresses adds freshly derived addresses to the pool. An
// address or index already present is skipped. Returns how many were added.
func AddCryptoPaymentAddresses(ctx context.Context, addresses []*CryptoPaymentAddress) (added int64) {
	if len(addresses) == 0 {
		return 0
	}
	server.Tx(ctx, func(tx server.PgTx) {
		added = 0
		for _, address := range addresses {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO crypto_payment_address (asset, address_index, address)
					VALUES ($1, $2, $3)
					ON CONFLICT DO NOTHING
				`,
				address.Asset,
				address.AddressIndex,
				address.Address,
			))
			added += tag.RowsAffected()
		}
	}, server.TxReadCommitted)
	return
}

// CountFreeCryptoPaymentAddresses is the coin's pool depth.
func CountFreeCryptoPaymentAddresses(ctx context.Context, asset string) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT count(*) FROM crypto_payment_address WHERE asset = $1 AND allocated_at IS NULL`,
			asset,
		).Scan(&count))
	})
	return
}

// MaxCryptoPaymentAddressIndex is the highest index the coin's pool holds,
// -1 when it is empty.
func MaxCryptoPaymentAddressIndex(ctx context.Context, asset string) (maxIndex int64) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COALESCE(max(address_index), -1) FROM crypto_payment_address WHERE asset = $1`,
			asset,
		).Scan(&maxIndex))
	})
	return
}

// GetCryptoPaymentPoolAddressAt is the coin's pool address with the lowest
// index (lowest = true) or the highest; ok = false when the pool is empty.
// The lowest is the wallet identity check's reference (an address derived
// long ago that a different wallet would not derive again); the highest is
// the one a Monero wallet must still own after a rebuild.
func GetCryptoPaymentPoolAddressAt(ctx context.Context, asset string, lowest bool) (addressIndex int64, address string, ok bool) {
	return GetCryptoPaymentPoolAddressAtInConn(nil, ctx, asset, lowest)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func GetCryptoPaymentPoolAddressAtInConn(connOwner server.PgConn, ctx context.Context, asset string, lowest bool) (addressIndex int64, address string, ok bool) {
	order := "DESC"
	if lowest {
		order = "ASC"
	}
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT address_index, address FROM crypto_payment_address WHERE asset = $1 ORDER BY address_index `+order+` LIMIT 1`,
			asset,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&addressIndex, &address))
				ok = true
			}
		})
	})
	return
}

// IsCryptoPaymentPoolAddress reports whether an address was derived for the
// pool, issued or not.
func IsCryptoPaymentPoolAddressInConn(connOwner server.PgConn, ctx context.Context, asset string, address string) (pool bool) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT 1 FROM crypto_payment_address WHERE asset = $1 AND address = $2`,
			asset,
			address,
		)
		server.WithPgResult(result, err, func() {
			pool = result.Next()
		})
	})
	return
}

// ----- scan cursor -----

// CryptoScanCursor is where a coin's scanner is.
type CryptoScanCursor struct {
	Asset         string
	Height        int64
	BlockHash     string
	WalletHeight  *int64
	NodeHeight    *int64
	NetworkHeight *int64
	LastSuccessAt *time.Time
	LastError     string
	UpdateTime    time.Time
}

// GetCryptoScanCursor reads the coin's cursor, or nil before its first scan.
func GetCryptoScanCursor(ctx context.Context, asset string) *CryptoScanCursor {
	return GetCryptoScanCursorInConn(nil, ctx, asset)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func GetCryptoScanCursorInConn(connOwner server.PgConn, ctx context.Context, asset string) (cursor *CryptoScanCursor) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT asset, height, block_hash, wallet_height, node_height, network_height,
				       last_success_at, last_error, update_time
				FROM crypto_scan_cursor
				WHERE asset = $1
			`,
			asset,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				cursor = &CryptoScanCursor{}
				var blockHash *string
				var lastError *string
				server.Raise(result.Scan(
					&cursor.Asset,
					&cursor.Height,
					&blockHash,
					&cursor.WalletHeight,
					&cursor.NodeHeight,
					&cursor.NetworkHeight,
					&cursor.LastSuccessAt,
					&lastError,
					&cursor.UpdateTime,
				))
				if blockHash != nil {
					cursor.BlockHash = *blockHash
				}
				if lastError != nil {
					cursor.LastError = *lastError
				}
			}
		})
	})
	return
}

// SetCryptoScanCursorSuccess records a complete scan: the height the next
// scan starts from (before its reorg window) and the heights it saw. A node
// or network height of 0 is unknown and stored as NULL: the quote path then
// treats the coin as unavailable, and nothing is orphaned on its word.
func SetCryptoScanCursorSuccessInConn(
	connOwner server.PgConn,
	ctx context.Context,
	asset string,
	height int64,
	walletHeight int64,
	nodeHeight int64,
	networkHeight int64,
	now time.Time,
) {
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO crypto_scan_cursor
				(asset, height, wallet_height, node_height, network_height, last_success_at, last_error, update_time)
				VALUES ($1, $2, $3, NULLIF($4, 0), NULLIF($5, 0), $6, NULL, $6)
				ON CONFLICT (asset) DO UPDATE
				SET height = EXCLUDED.height,
					wallet_height = EXCLUDED.wallet_height,
					node_height = EXCLUDED.node_height,
					network_height = EXCLUDED.network_height,
					last_success_at = EXCLUDED.last_success_at,
					last_error = NULL,
					update_time = EXCLUDED.update_time
			`,
			asset,
			height,
			walletHeight,
			nodeHeight,
			networkHeight,
			now,
		))
	}, server.TxReadCommitted)
}

// SetCryptoScanCursorError records why the coin's last scan failed. The
// cursor and the last success stay where they were.
func SetCryptoScanCursorErrorInConn(connOwner server.PgConn, ctx context.Context, asset string, lastError string, now time.Time) {
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO crypto_scan_cursor (asset, last_error, update_time)
				VALUES ($1, $2, $3)
				ON CONFLICT (asset) DO UPDATE
				SET last_error = EXCLUDED.last_error, update_time = EXCLUDED.update_time
			`,
			asset,
			lastError,
			now,
		))
	}, server.TxReadCommitted)
}

// ----- crediting a balance code inside the caller's transaction -----

// CreateBalanceCodeInTx is CreateBalanceCode in the caller's transaction, for
// a credit that must create its code atomically with the rest of the credit.
// The purchase event id is unique: a second call for one event creates no
// second code and returns the first (created = false).
func CreateBalanceCodeInTx(
	tx server.PgTx,
	ctx context.Context,
	balanceByteCount ByteCount,
	duration time.Duration,
	netRevenue NanoCents,
	purchaseEventId string,
	purchaseRecord string,
	purchaseEmail string,
) (balanceCode *BalanceCode, created bool, returnErr error) {
	var existingId *server.Id
	result, err := tx.Query(
		ctx,
		`SELECT balance_code_id FROM transfer_balance_code WHERE purchase_event_id = $1`,
		purchaseEventId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&existingId))
		}
	})
	if existingId != nil {
		balanceCode = &BalanceCode{}
		result, err := tx.Query(
			ctx,
			`
				SELECT create_time, start_time, end_time, balance_byte_count, net_revenue_nano_cents,
				       balance_code_secret, purchase_event_id, purchase_record, purchase_email
				FROM transfer_balance_code
				WHERE balance_code_id = $1
			`,
			*existingId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				balanceCode.BalanceCodeId = *existingId
				server.Raise(result.Scan(
					&balanceCode.CreateTime,
					&balanceCode.StartTime,
					&balanceCode.EndTime,
					&balanceCode.BalanceByteCount,
					&balanceCode.NetRevenue,
					&balanceCode.Secret,
					&balanceCode.PurchaseEventId,
					&balanceCode.PurchaseRecord,
					&balanceCode.PurchaseEmail,
				))
			}
		})
		return balanceCode, false, nil
	}

	secret, err := newCodeBase32()
	if err != nil {
		return nil, false, err
	}
	createTime := server.NowUtc()
	// the same window CreateBalanceCode gives a code: from 00:00 the day it is
	// made to 00:00 the day after its duration ends
	startTime := time.Date(createTime.Year(), createTime.Month(), createTime.Day(), 0, 0, 0, 0, createTime.Location())
	endTime := startTime.Add(24 * time.Hour).Add(duration)
	balanceCodeId := server.NewId()
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO transfer_balance_code (
				balance_code_id,
				create_time,
				start_time,
				end_time,
				balance_byte_count,
				net_revenue_nano_cents,
				balance_code_secret,
				purchase_event_id,
				purchase_record,
				purchase_email
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`,
		balanceCodeId,
		createTime,
		startTime,
		endTime,
		balanceByteCount,
		netRevenue,
		secret,
		purchaseEventId,
		purchaseRecord,
		purchaseEmail,
	))
	return &BalanceCode{
		BalanceCodeId:    balanceCodeId,
		CreateTime:       createTime,
		StartTime:        startTime,
		EndTime:          endTime,
		BalanceByteCount: balanceByteCount,
		NetRevenue:       netRevenue,
		Secret:           secret,
		PurchaseEventId:  purchaseEventId,
		PurchaseRecord:   purchaseRecord,
		PurchaseEmail:    purchaseEmail,
	}, true, nil
}

// GetBalanceCodeSecretInConn is the secret of a balance code, for the status
// of the invoice that created it.
func GetBalanceCodeSecretInConn(connOwner server.PgConn, ctx context.Context, balanceCodeId server.Id) (secret string) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT balance_code_secret FROM transfer_balance_code WHERE balance_code_id = $1`,
			balanceCodeId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&secret))
			}
		})
	})
	return
}

// ----- ending what a credit granted -----

// EndCryptoPaymentGrantInTx ends, at now, what an invoice's credit granted
// and is still running: a plan's renewal and Pro balance (by the renewal's
// transaction id), an account or network data pack's balance, or a balance
// code (voided when unredeemed, its balance ended when redeemed). Returns the
// networks whose balances it ended, which the caller refreshes the Pro cache
// of after commit, and whether it ended anything at all (a voided code ends
// no network's balance). The caller's transaction must use OptNoRetry: a
// balance owned by another writer aborts it (errTransferBalanceOwnershipBusy)
// to retry whole.
func EndCryptoPaymentGrantInTx(
	tx server.PgTx,
	ctx context.Context,
	intent *CryptoPaymentIntent,
	market SubscriptionMarket,
	renewalTransactionId string,
	plan bool,
	now time.Time,
) (endedNetworkIds []server.Id, ended bool) {
	if plan {
		if renewalTransactionId != "" {
			endedNetworkIds = append(endedNetworkIds, EndReconciledEntitlementForTransactionsInTx(
				tx, ctx, market, []string{renewalTransactionId}, now,
			)...)
		}
		ended = 0 < len(endedNetworkIds)
		return
	}
	if intent.BalanceCodeId != nil {
		_, voidedCount, endedNetworks := ClawbackBalanceCodesForPurchaseEventInTx(tx, ctx, CryptoPaymentPurchaseEventId(intent.IntentId), now)
		endedNetworkIds = append(endedNetworkIds, endedNetworks...)
		ended = 0 < voidedCount || 0 < len(endedNetworks)
		return
	}
	if intent.BalanceId != nil {
		admitted, err := tryTransferBalanceOwnershipInTx(ctx, tx, []server.Id{*intent.BalanceId})
		server.Raise(err)
		if !admitted {
			server.Raise(errTransferBalanceOwnershipBusy)
		}
		result, err := tx.Query(
			ctx,
			`
				UPDATE transfer_balance
				SET end_time = $2
				WHERE balance_id = $1 AND end_time > $2
				RETURNING network_id
			`,
			*intent.BalanceId,
			now,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				endedNetworkIds = append(endedNetworkIds, networkId)
			}
		})
		ended = 0 < len(endedNetworkIds)
	}
	return
}

// CryptoPaymentPurchaseEventId is the balance-code purchase event of an
// invoice's code: one code per invoice.
func CryptoPaymentPurchaseEventId(intentId server.Id) string {
	return "crypto/" + intentId.String()
}

// ----- the retention unlink (decision 9) -----

// CryptoPaymentUnlinkResult is what one unlink pass did.
type CryptoPaymentUnlinkResult struct {
	Unlinked int
}

// UnlinkCryptoPaymentIntents drops the link between invoices and the networks
// they were for (decision 9, mmm/ZCASH-CONTRACT.md §3.13.6): invoices credited before
// cutoff, and invoices never credited that were created before it. For each,
// in one transaction:
//   - the renewal's transaction id becomes a fresh random id;
//   - a USDC invoice's Solana intent loses its network, and its times are
//     truncated to the day;
//   - a code's purchase event id stops naming the invoice, and its purchase
//     record and email are cleared;
//   - the invoice forgets its network, email and grant handles, and its times
//     are truncated to the day, as are its receipts' and its pool address's;
//     its quote history is deleted (the invoice keeps the final quote);
//   - the reconciliation events about it (by the invoice id, or one of its
//     receipts' transactions) lose their network.
//
// The transaction ids, amounts and days stay for accounting. Someone with
// database access could still correlate an amount and a day with a
// subscription record; the launch notes say so. One batch per call, oldest
// first; the caller repeats while a batch comes back full.
func UnlinkCryptoPaymentIntents(ctx context.Context, cutoff time.Time, limit int, now time.Time) (unlinked int) {
	server.Tx(ctx, func(tx server.PgTx) {
		unlinked = 0
		type unlinkRow struct {
			intentId        server.Id
			asset           string
			networkId       *server.Id
			solanaReference *string
		}
		rows := []*unlinkRow{}
		result, err := tx.Query(
			ctx,
			`
				SELECT intent_id, asset, network_id, solana_reference
				FROM crypto_payment_intent
				WHERE intent_id IN (
					(
						SELECT intent_id FROM crypto_payment_intent
						WHERE credited_at IS NOT NULL AND unlinked_at IS NULL AND credited_at < $1
						ORDER BY credited_at
						LIMIT $2
					)
					UNION ALL
					(
						SELECT intent_id FROM crypto_payment_intent
						WHERE credited_at IS NULL AND unlinked_at IS NULL AND create_time < $1
						ORDER BY create_time
						LIMIT $2
					)
				)
				  AND unlinked_at IS NULL
				LIMIT $2
				FOR UPDATE SKIP LOCKED
			`,
			cutoff,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &unlinkRow{}
				server.Raise(result.Scan(&row.intentId, &row.asset, &row.networkId, &row.solanaReference))
				rows = append(rows, row)
			}
		})
		for _, row := range rows {
			if row.networkId != nil {
				market, transactionId := cryptoPaymentRenewalHandle(row.asset, row.intentId, row.solanaReference)
				if market != "" && transactionId != "" {
					server.RaisePgResult(tx.Exec(
						ctx,
						`
							UPDATE subscription_renewal
							SET transaction_id = $4
							WHERE network_id = $1 AND market = $2 AND transaction_id = $3
						`,
						*row.networkId,
						market,
						transactionId,
						server.NewId().String(),
					))
				}
			}
			if row.solanaReference != nil {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						UPDATE solana_payment_intent
						SET network_id = NULL,
							created_at = date_trunc('day', created_at),
							expires_at = date_trunc('day', expires_at)
						WHERE payment_reference = $1
					`,
					*row.solanaReference,
				))
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE transfer_balance_code
					SET purchase_event_id = $2, purchase_record = '', purchase_email = ''
					WHERE purchase_event_id = $1
				`,
				CryptoPaymentPurchaseEventId(row.intentId),
				"crypto/unlinked/"+server.NewId().String(),
			))
			// the events name the invoice, or one of its receipts' transactions
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE payment_reconciliation_event
					SET network_id = NULL
					WHERE network_id IS NOT NULL
					  AND (store IN ('monero', 'zcash') OR (store = 'solana' AND action = 'refunded'))
					  AND (
						evidence = $1
						OR evidence IN (SELECT tx_id FROM crypto_payment_receipt WHERE intent_id = $2)
					  )
				`,
				row.intentId.String(),
				row.intentId,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE crypto_payment_receipt
					SET first_seen_at = date_trunc('day', first_seen_at),
						confirmed_at = date_trunc('day', confirmed_at),
						final_at = date_trunc('day', final_at),
						update_time = date_trunc('day', update_time)
					WHERE intent_id = $1
				`,
				row.intentId,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE crypto_payment_address
					SET allocated_at = date_trunc('day', allocated_at)
					WHERE intent_id = $1
				`,
				row.intentId,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM crypto_payment_quote WHERE intent_id = $1`,
				row.intentId,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE crypto_payment_intent
					SET network_id = NULL,
						email = NULL,
						balance_id = NULL,
						balance_code_id = NULL,
						quoted_at = date_trunc('day', quoted_at),
						expires_at = date_trunc('day', expires_at),
						create_time = date_trunc('day', create_time),
						credited_at = date_trunc('day', credited_at),
						voided_at = date_trunc('day', voided_at),
						refunded_at = date_trunc('day', refunded_at),
						unlinked_at = date_trunc('day', $2::timestamp)
					WHERE intent_id = $1
				`,
				row.intentId,
				now,
			))
			unlinked += 1
		}
		// and every crypto store's event older than the cutoff, whatever it
		// names
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE payment_reconciliation_event
				SET network_id = NULL
				WHERE store IN ('monero', 'zcash') AND network_id IS NOT NULL AND event_time < $1
			`,
			cutoff,
		))
	}, server.TxReadCommitted)
	return
}

// cryptoPaymentRenewalHandle is where a plan credit's renewal is found: its
// market and transaction id (the invoice id for xmr and zec, the Solana
// reference for USDC so the Solana reconcile leg still finds it).
func cryptoPaymentRenewalHandle(asset string, intentId server.Id, solanaReference *string) (SubscriptionMarket, string) {
	switch asset {
	case CryptoAssetXmr:
		return SubscriptionMarketMonero, intentId.String()
	case CryptoAssetZec:
		return SubscriptionMarketZcash, intentId.String()
	case CryptoAssetUsdcSolana:
		if solanaReference != nil {
			return SubscriptionMarketSolana, *solanaReference
		}
	}
	return "", ""
}

// CryptoPaymentRenewalHandle exports cryptoPaymentRenewalHandle for the
// controller.
func CryptoPaymentRenewalHandle(intent *CryptoPaymentIntent) (SubscriptionMarket, string) {
	return cryptoPaymentRenewalHandle(intent.Asset, intent.IntentId, intent.SolanaReference)
}

// MarshalCryptoRateSources is the json kept with a quote.
func MarshalCryptoRateSources(sources any) string {
	if sources == nil {
		return ""
	}
	sourcesJson, err := json.Marshal(sources)
	if err != nil {
		return ""
	}
	return string(sourcesJson)
}

// GetLatestOneTimeSubscriptionRenewal is the network's supporter renewal in
// one of the given one-time markets (solana, monero, zcash) that ends last and
// has not ended at now. ok = false when there is none.
func GetLatestOneTimeSubscriptionRenewal(
	ctx context.Context,
	networkId server.Id,
	markets []string,
	now time.Time,
) (market string, endTime time.Time, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT market, end_time
				FROM subscription_renewal
				WHERE network_id = $1
				  AND subscription_type = $2
				  AND market = ANY($3)
				  AND $4 < end_time
				ORDER BY end_time DESC
				LIMIT 1
			`,
			networkId,
			SubscriptionTypeSupporter,
			markets,
			now,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&market, &endTime))
				ok = true
			}
		})
	})
	return
}
