package model

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// The Solana subscription plans. These are recorded on the intent so the webhook can
// grant what was actually bought, instead of assuming.
const (
	SolanaPlanMonthly = "monthly"
	SolanaPlanYearly  = "yearly"
	// the welcome offer: a year at the tier's yearly price less the onboarding
	// discount, plus the 14-day trial, only while the network's offer is redeemable
	SolanaPlanYearlyOnboarding = "yearly_onboarding"
)

// CreateSolanaPaymentIntent records what the customer was QUOTED: the price shown to
// them, and the plan they picked.
//
// Without this the webhook had nothing to check an arriving payment against, so it
// hardcoded `>= 40 USDC` and always granted a year. The $5 monthly option on the site
// therefore took the money and delivered nothing (5 < 40, ignored as "no matching USDC
// payment"), while any payment of 40+ bought a year regardless of size.
func CreateSolanaPaymentIntent(
	reference string,
	expectedAmountUsd float64,
	subscriptionPlan string,
	session *session.ClientSession,
) (err error) {

	server.Tx(session.Ctx, func(tx server.PgTx) {

		// a failed insert raises, which ends the transaction at once. It used
		// to set the error result while the transaction went on to a commit
		// that server.Tx retried for a minute.
		tag := server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				INSERT INTO solana_payment_intent
				(payment_reference, network_id, expires_at, expected_amount_usd, subscription_plan)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT DO NOTHING
			`,
			reference,
			session.ByJwt.NetworkId,
			server.NowUtc().Add(1*time.Hour),
			expectedAmountUsd,
			subscriptionPlan,
		))
		if tag.RowsAffected() == 0 {
			err = errors.New("payment_reference already exists")
			return
		}

	})

	return

}

/**
 * The Helius webhook returns an array of accounts
 * There is no indication which is the reference id, so we have to search them all
 */

type PaymentIntentSearchResult struct {
	NetworkId        *server.Id `json:"network_id"`
	PaymentReference string     `json:"payment_reference"`
	// what the customer was quoted, and what they picked. 0 / "" for intents created
	// before these were recorded: the webhook then falls back to the old behavior.
	ExpectedAmountUsd float64 `json:"expected_amount_usd"`
	SubscriptionPlan  string  `json:"subscription_plan"`
}

func SearchPaymentIntents(
	references []string,
	session *session.ClientSession,
) (*PaymentIntentSearchResult, error) {
	return SearchPaymentIntentsInConn(nil, references, session)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func SearchPaymentIntentsInConn(connOwner server.PgConn,
	references []string,
	session *session.ClientSession,
) (*PaymentIntentSearchResult, error) {

	var paymentIntent *PaymentIntentSearchResult

	server.TxInConn(session.Ctx, connOwner, func(tx server.PgTx) {

		result, err := tx.Query(
			session.Ctx,
			`
			SELECT payment_reference, network_id, expected_amount_usd, subscription_plan
		    FROM solana_payment_intent
		    WHERE tx_signature IS NULL
		      AND payment_reference = ANY($1)
		    LIMIT 1
			`,
			references,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				paymentIntent = &PaymentIntentSearchResult{}
				server.Raise(result.Scan(
					&paymentIntent.PaymentReference,
					&paymentIntent.NetworkId,
					&paymentIntent.ExpectedAmountUsd,
					&paymentIntent.SubscriptionPlan,
				))
			}
		})
	})

	return paymentIntent, nil

}

// MarkPaymentIntentCompletedInTx consumes the intent. completed = false means the
// intent was already consumed (or never existed) -- the caller must NOT credit.
//
// The `tx_signature IS NULL` predicate is the concurrency gate: two deliveries of
// the same transaction both set the SAME signature, so the unique index on
// tx_signature never fires. Under READ COMMITTED the second UPDATE blocks on the
// row lock, re-evaluates the predicate after the first commits, and affects zero
// rows -- exactly one delivery wins the credit.
func MarkPaymentIntentCompletedInTx(
	tx server.PgTx,
	reference string,
	signature string,
	session *session.ClientSession,
) (completed bool, err error) {
	// a failed update aborts the caller's transaction, so it raises rather
	// than leave the credit path to commit a rollback
	tag := server.RaisePgResult(tx.Exec(
		session.Ctx,
		`
		UPDATE solana_payment_intent
		SET tx_signature = $1
		WHERE payment_reference = $2
		  AND tx_signature IS NULL
		`,
		signature,
		reference,
	))
	return tag.RowsAffected() != 0, nil
}

func MarkPaymentIntentCompleted(
	reference string,
	signature string,
	session *session.ClientSession,
) (completed bool, err error) {

	server.Tx(session.Ctx, func(tx server.PgTx) {
		completed, err = MarkPaymentIntentCompletedInTx(tx, reference, signature, session)
	})

	return

}

// IsSolanaPaymentCompleted reports whether a transaction signature has already
// consumed an intent -- i.e. this exact on-chain payment has already been credited.
// The webhook uses it to tell a REDELIVERY of a credited payment (nothing to do)
// apart from a payment that never matched an intent (record it for an operator).
func IsSolanaPaymentCompleted(
	ctx context.Context,
	signature string,
) (completed bool) {
	return IsSolanaPaymentCompletedInConn(nil, ctx, signature)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func IsSolanaPaymentCompletedInConn(connOwner server.PgConn,
	ctx context.Context,
	signature string,
) (completed bool) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT 1 FROM solana_payment_intent
			WHERE tx_signature = $1
			`,
			signature,
		)
		server.WithPgResult(result, err, func() {
			completed = result.Next()
		})
	})
	return
}

// The reasons a received payment could not be fulfilled -- see
// RecordUnfulfilledSolanaPayment.
const (
	SolanaUnfulfilledReasonNoIntent  = "no_intent"
	SolanaUnfulfilledReasonUnderpaid = "underpaid"
)

// UnfulfilledSolanaPayment records a USDC payment that arrived at one of our
// receiving addresses but bought nothing: either no open intent matched any of
// the transaction's account keys (a late payment whose intent was already swept,
// or an unknown reference), or the amount was under the quote. The money moved
// on-chain and Helius is acked 200 either way (it never re-examines a delivered
// tx), so this row is the operator-visible record with enough detail to repair
// by hand.
type UnfulfilledSolanaPayment struct {
	TxSignature    string
	Reason         string
	TokenAmountUsd float64
	// what the payment was checked against, when an intent matched (underpaid)
	ExpectedAmountUsd *float64
	PaymentReference  *string
	NetworkId         *server.Id
	// the account keys the reference was searched among, when none matched
	ReferenceCandidates []string
	// the on-chain timestamp, when the webhook carried one
	TransactionTime *time.Time
	// the wallet that sent the payment, when it was a single transfer
	SenderAccount *string
	// why a payment without a reference was not matched by its amount, with
	// the candidate intents, for support to credit by hand
	MatchNote *string
}

// RecordUnfulfilledSolanaPayment writes the row. Idempotent on the tx signature
// (ON CONFLICT DO NOTHING), so a Helius redelivery does not duplicate it. Never
// raises: recording must not turn an acked payment into a retry loop.
func RecordUnfulfilledSolanaPayment(
	ctx context.Context,
	payment *UnfulfilledSolanaPayment,
) (err error) {

	var referenceCandidates *string
	if 0 < len(payment.ReferenceCandidates) {
		candidatesJson, jsonErr := json.Marshal(payment.ReferenceCandidates)
		if jsonErr == nil {
			candidatesJsonStr := string(candidatesJson)
			referenceCandidates = &candidatesJsonStr
		}
	}

	// a failed insert raises, which ends the transaction at once; the error
	// result is always nil. It used to be assigned to the result while the
	// transaction went on to a commit that server.Tx retried for a minute.
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO solana_unfulfilled_payment
			(tx_signature, reason, token_amount_usd, expected_amount_usd,
			 payment_reference, network_id, reference_candidates, transaction_time,
			 sender_account, match_note)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT DO NOTHING
			`,
			payment.TxSignature,
			payment.Reason,
			payment.TokenAmountUsd,
			payment.ExpectedAmountUsd,
			payment.PaymentReference,
			payment.NetworkId,
			referenceCandidates,
			payment.TransactionTime,
			payment.SenderAccount,
			payment.MatchNote,
		))
	})

	return
}

// ListUnfulfilledSolanaPayments returns recorded payments with the given
// reason, oldest first, with the reference candidates decoded -- the shape the
// reconciler sweeps: a no_intent payment whose reference NOW resolves to an
// open intent can be credited after all.
func ListUnfulfilledSolanaPayments(
	ctx context.Context,
	reason string,
	limit int,
) []*UnfulfilledSolanaPayment {
	return ListUnfulfilledSolanaPaymentsInConn(nil, ctx, reason, limit)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func ListUnfulfilledSolanaPaymentsInConn(connOwner server.PgConn,
	ctx context.Context,
	reason string,
	limit int,
) []*UnfulfilledSolanaPayment {
	payments := []*UnfulfilledSolanaPayment{}
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT tx_signature, token_amount_usd, expected_amount_usd,
			       payment_reference, network_id, reference_candidates
			FROM solana_unfulfilled_payment
			WHERE reason = $1
			ORDER BY record_time ASC
			LIMIT $2
			`,
			reason,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				payment := &UnfulfilledSolanaPayment{
					Reason: reason,
				}
				var referenceCandidates *string
				server.Raise(result.Scan(
					&payment.TxSignature,
					&payment.TokenAmountUsd,
					&payment.ExpectedAmountUsd,
					&payment.PaymentReference,
					&payment.NetworkId,
					&referenceCandidates,
				))
				if referenceCandidates != nil {
					json.Unmarshal([]byte(*referenceCandidates), &payment.ReferenceCandidates)
				}
				payments = append(payments, payment)
			}
		})
	})
	return payments
}

// RemoveUnfulfilledSolanaPayment clears a recorded payment once it is no
// longer unfulfilled (the reconciler credited it, or found it already
// credited by a redelivery).
func RemoveUnfulfilledSolanaPayment(
	ctx context.Context,
	txSignature string,
) (err error) {
	return RemoveUnfulfilledSolanaPaymentInConn(nil, ctx, txSignature)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func RemoveUnfulfilledSolanaPaymentInConn(connOwner server.PgConn,
	ctx context.Context,
	txSignature string,
) (err error) {
	// a failed delete raises, which ends the transaction at once; the error
	// result is always nil
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM solana_unfulfilled_payment
			WHERE tx_signature = $1
			`,
			txSignature,
		))
	})
	return
}

// GetSolanaPaymentIntentSignature returns the tx signature a completed intent
// was consumed by. ok = false means the intent does not exist or is still
// open. The reconciler uses this to re-verify credited payments on-chain.
func GetSolanaPaymentIntentSignature(
	ctx context.Context,
	reference string,
) (signature string, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT tx_signature FROM solana_payment_intent
			WHERE payment_reference = $1
			  AND tx_signature IS NOT NULL
			`,
			reference,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&signature))
				ok = true
			}
		})
	})
	return
}

// GetSolanaPaymentIntentCompletion returns the immutable local owner and chain
// signature of a consumed intent. Reconciliation uses both fields before a
// provider verdict can repair entitlement metadata; a valid signature owned
// by a different network is never authority for this renewal.
func GetSolanaPaymentIntentCompletion(
	ctx context.Context,
	reference string,
) (networkId server.Id, signature string, ok bool) {
	return GetSolanaPaymentIntentCompletionInConn(nil, ctx, reference)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func GetSolanaPaymentIntentCompletionInConn(connOwner server.PgConn,
	ctx context.Context,
	reference string,
) (networkId server.Id, signature string, ok bool) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT network_id, tx_signature
			FROM solana_payment_intent
			WHERE payment_reference = $1
			  AND tx_signature IS NOT NULL
			`,
			reference,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId, &signature))
				ok = true
			}
		})
	})
	return
}

// GetUnfulfilledSolanaPayment reads one recorded payment back by signature.
func GetUnfulfilledSolanaPayment(
	ctx context.Context,
	txSignature string,
) (payment *UnfulfilledSolanaPayment, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT reason, token_amount_usd, expected_amount_usd,
			       payment_reference, network_id, sender_account, match_note
			FROM solana_unfulfilled_payment
			WHERE tx_signature = $1
			`,
			txSignature,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				payment = &UnfulfilledSolanaPayment{
					TxSignature: txSignature,
				}
				server.Raise(result.Scan(
					&payment.Reason,
					&payment.TokenAmountUsd,
					&payment.ExpectedAmountUsd,
					&payment.PaymentReference,
					&payment.NetworkId,
					&payment.SenderAccount,
					&payment.MatchNote,
				))
			} else {
				returnErr = errors.New("Unfulfilled payment not found.")
			}
		})
	})
	return
}

// todo - create a task to cleanup expired intents without a tx_signature
func CleanupExpiredPaymentIntents(
	ctx context.Context,
	minTime time.Time,
) (err error) {

	server.MaintenanceTx(ctx, func(tx server.PgTx) {

		_, err = tx.Exec(
			ctx,
			`
			DELETE FROM solana_payment_intent
			WHERE expires_at < $1
			  AND tx_signature IS NULL
			`,
			minTime,
		)
		server.Raise(err)

	})

	return

}

// CreateSolanaPaymentIntentForNetwork records a quote for a network that is
// not the caller's: the buy-data page sells USDC data packs to a NAMED network
// with no sign-in (controller.PayDataSolanaIntent). subscriptionPlan carries the
// data item id ("data_1tib") there, which is how the webhook tells a data pack
// from a plan when it credits. The expiry is the caller's: a payment sent by
// hand takes longer than a wallet flow.
func CreateSolanaPaymentIntentForNetwork(
	ctx context.Context,
	reference string,
	networkId server.Id,
	expectedAmountUsd float64,
	subscriptionPlan string,
	expiresAt time.Time,
) (err error) {
	server.Tx(ctx, func(tx server.PgTx) {
		// a failed insert raises, which ends the transaction at once
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO solana_payment_intent
				(payment_reference, network_id, expires_at, expected_amount_usd, subscription_plan)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT DO NOTHING
			`,
			reference,
			networkId,
			expiresAt,
			expectedAmountUsd,
			subscriptionPlan,
		))
		if tag.RowsAffected() == 0 {
			err = errors.New("payment_reference already exists")
			return
		}
	})
	return
}

// SolanaPaymentIntent is one intent row, open or consumed.
type SolanaPaymentIntent struct {
	PaymentReference  string
	NetworkId         server.Id
	ExpectedAmountUsd float64
	SubscriptionPlan  string
	CreatedAt         time.Time
	ExpiresAt         *time.Time
	// set once the intent was consumed by an on-chain payment
	TxSignature *string
}

// GetSolanaPaymentIntent reads one intent back by reference, consumed or not.
// nil when there is none (or it was swept after expiring).
func GetSolanaPaymentIntent(
	ctx context.Context,
	reference string,
) (intent *SolanaPaymentIntent) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				payment_reference,
				network_id,
				expected_amount_usd,
				subscription_plan,
				created_at,
				expires_at,
				tx_signature
			FROM solana_payment_intent
			WHERE payment_reference = $1
			`,
			reference,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				intent = &SolanaPaymentIntent{}
				var expectedAmountUsd *float64
				var subscriptionPlan *string
				server.Raise(result.Scan(
					&intent.PaymentReference,
					&intent.NetworkId,
					&expectedAmountUsd,
					&subscriptionPlan,
					&intent.CreatedAt,
					&intent.ExpiresAt,
					&intent.TxSignature,
				))
				if expectedAmountUsd != nil {
					intent.ExpectedAmountUsd = *expectedAmountUsd
				}
				if subscriptionPlan != nil {
					intent.SubscriptionPlan = *subscriptionPlan
				}
			}
		})
	})
	return
}

// Unique quote amounts for memo-less payments.
//
// A buyer whose wallet cannot add a memo sends the quoted amount with no
// reference. To identify the intent from the amount alone, every new quote is
// the price plus a sub-cent suffix of 1 to SolanaUniqueAmountMaxSuffixMicro
// micro-USDC (USDC has 6 decimals), and that exact amount is reserved for the
// intent in solana_payment_amount_reservation until the intent's expiry plus
// SolanaUniqueAmountHold. No two intents hold the same amount at once, and an
// amount is not quoted again until a late payment of the previous quote is
// implausible. When no suffix is free the quote is the plain price and the
// intent matches by reference only, as before.
const (
	SolanaUsdcMicroPerUsd = 1_000_000
	// the suffix stays under a cent
	SolanaUniqueAmountMaxSuffixMicro = 9_999
	// how long an amount stays reserved after its intent expires
	SolanaUniqueAmountHold = 30 * 24 * time.Hour
	// random suffixes tried before quoting the plain price
	solanaUniqueAmountAttempts = 32
)

// SolanaUsdToMicro is the exact micro-USDC of a usd amount. Rounded, so float
// dust in a quote or a chain-reported token amount never shifts it.
func SolanaUsdToMicro(amountUsd float64) int64 {
	return int64(math.Round(amountUsd * SolanaUsdcMicroPerUsd))
}

// SolanaMicroToUsd is the usd amount of exact micro-USDC.
func SolanaMicroToUsd(amountMicro int64) float64 {
	return float64(amountMicro) / SolanaUsdcMicroPerUsd
}

// SolanaUniqueAmountMicro is the quote for a price with a suffix. ok = false
// for a suffix out of range or a non-positive price.
func SolanaUniqueAmountMicro(priceUsd float64, suffixMicro int64) (amountMicro int64, ok bool) {
	if priceUsd <= 0 || suffixMicro < 1 || SolanaUniqueAmountMaxSuffixMicro < suffixMicro {
		return 0, false
	}
	return SolanaUsdToMicro(priceUsd) + suffixMicro, true
}

// CreateSolanaPaymentIntentWithUniqueAmount records a quote of priceUsd plus a
// reserved suffix and returns the amount the buyer must send. pickSuffixMicro
// returns a candidate suffix in [1, SolanaUniqueAmountMaxSuffixMicro]; it is
// injected so tests are deterministic.
//
// The intent row is inserted first, so a duplicate reference reserves nothing.
// A reservation is taken only when the amount is free or its previous hold has
// lapsed (ON CONFLICT ... WHERE reserved_until < now): the primary key on the
// amount makes concurrent quotes of the same amount exclusive. With no free
// suffix after solanaUniqueAmountAttempts tries the intent keeps the plain
// price and no expected_amount_micro, so it can only match by reference.
//
// created_at is set here from the same clock as expires_at: the memo-less
// match compares the on-chain time against both.
func CreateSolanaPaymentIntentWithUniqueAmount(
	ctx context.Context,
	reference string,
	networkId server.Id,
	priceUsd float64,
	subscriptionPlan string,
	expiresAt time.Time,
	pickSuffixMicro func() int64,
) (amountUsd float64, err error) {
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		amountUsd = 0
		err = nil
		// a failed statement raises, which ends the transaction at once. Each
		// used to set the error result while the transaction went on to a
		// commit that server.Tx retried for a minute.
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO solana_payment_intent
				(payment_reference, network_id, created_at, expires_at, expected_amount_usd, subscription_plan)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT DO NOTHING
			`,
			reference,
			networkId,
			now,
			expiresAt,
			priceUsd,
			subscriptionPlan,
		))
		if tag.RowsAffected() == 0 {
			err = errors.New("payment_reference already exists")
			return
		}
		amountUsd = priceUsd

		for range solanaUniqueAmountAttempts {
			amountMicro, ok := SolanaUniqueAmountMicro(priceUsd, pickSuffixMicro())
			if !ok {
				continue
			}
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO solana_payment_amount_reservation
					(amount_micro, payment_reference, reserved_until)
					VALUES ($1, $2, $3)
					ON CONFLICT (amount_micro) DO UPDATE
					SET payment_reference = EXCLUDED.payment_reference,
						reserved_until = EXCLUDED.reserved_until
					WHERE solana_payment_amount_reservation.reserved_until < $4
				`,
				amountMicro,
				reference,
				expiresAt.Add(SolanaUniqueAmountHold),
				now,
			))
			if tag.RowsAffected() == 0 {
				// held by another quote
				continue
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE solana_payment_intent
					SET expected_amount_usd = $2,
						expected_amount_micro = $3
					WHERE payment_reference = $1
				`,
				reference,
				SolanaMicroToUsd(amountMicro),
				amountMicro,
			))
			amountUsd = SolanaMicroToUsd(amountMicro)
			return
		}
	})
	if err != nil {
		return 0, err
	}
	return amountUsd, nil
}

// ListSolanaPaymentIntentsByAmountMicro returns every intent, open or consumed,
// quoted exactly amountMicro that expires after minExpiresAt. The memo-less
// match passes the payment time less SolanaUniqueAmountHold, so the result
// holds every intent the amount could belong to; more than one is ambiguous
// and is not credited.
func ListSolanaPaymentIntentsByAmountMicro(
	ctx context.Context,
	amountMicro int64,
	minExpiresAt time.Time,
) (intents []*SolanaPaymentIntent, returnErr error) {
	intents = []*SolanaPaymentIntent{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				payment_reference,
				network_id,
				expected_amount_usd,
				subscription_plan,
				created_at,
				expires_at,
				tx_signature
			FROM solana_payment_intent
			WHERE expected_amount_micro = $1
			  AND $2 < expires_at
			LIMIT 8
			`,
			amountMicro,
			minExpiresAt,
		)
		if err != nil {
			returnErr = err
			return
		}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				intent := &SolanaPaymentIntent{}
				server.Raise(result.Scan(
					&intent.PaymentReference,
					&intent.NetworkId,
					&intent.ExpectedAmountUsd,
					&intent.SubscriptionPlan,
					&intent.CreatedAt,
					&intent.ExpiresAt,
					&intent.TxSignature,
				))
				intents = append(intents, intent)
			}
		})
	})
	return
}
