package model

// Database-backed test of the received-but-uncredited payments in the
// subscriptions snapshot (subscriptionMetricsUncreditedQuery): per rail, the
// payments that reached us at least the minimum age ago
// and that no credit consumed, with the oldest's age. Needs the test database
// (server.DefaultTestEnv).

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Per rail, the payments past the minimum age that no credit consumed, with
// the oldest's age; credited, orphaned, ignored and young ones are not
// counted.
func TestLoadSubscriptionMetricsSnapshotCountsReceivedPaymentsNeverCredited(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		// USDC into a Solana receiver that bought nothing: one two hours
		// old, one ten minutes old (still the webhook's and the legs')
		for _, payment := range []struct {
			signature string
			age       time.Duration
		}{
			{"uncredited-old", 2 * time.Hour},
			{"uncredited-young", 10 * time.Minute},
		} {
			transactionTime := now.Add(-payment.age)
			sender := "synthetic-sender"
			err := RecordUnfulfilledSolanaPayment(ctx, &UnfulfilledSolanaPayment{
				TxSignature:     payment.signature,
				Reason:          SolanaUnfulfilledReasonNoIntent,
				TokenAmountUsd:  40,
				TransactionTime: &transactionTime,
				SenderAccount:   &sender,
			})
			connect.AssertEqual(t, err, nil)
		}

		receipt := func(asset string, txId string, state string, firstSeen time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO crypto_payment_receipt
						(asset, tx_id, output_key, address, address_index, amount_atomic,
						 confirmations, first_seen_at, state)
						VALUES ($1, $2, '0', 'synthetic-address', 7, 1000, 1, $3, $4)
					`,
					asset,
					txId,
					firstSeen,
					state,
				))
			})
		}
		// a Monero payment in a block for an hour, a Zcash one held for
		// review: uncredited
		receipt(CryptoAssetXmr, "xmr-confirmed", CryptoReceiptStateConfirmed, now.Add(-time.Hour))
		receipt(CryptoAssetZec, "zec-review", CryptoReceiptStateReview, now.Add(-3*time.Hour))
		// credited, gone from the chain, never counted, or young: not
		receipt(CryptoAssetXmr, "xmr-credited", CryptoReceiptStateCredited, now.Add(-time.Hour))
		receipt(CryptoAssetXmr, "xmr-orphaned", CryptoReceiptStateOrphaned, now.Add(-time.Hour))
		receipt(CryptoAssetZec, "zec-ignored", CryptoReceiptStateIgnored, now.Add(-time.Hour))
		receipt(CryptoAssetXmr, "xmr-young", CryptoReceiptStateSeen, now.Add(-5*time.Minute))

		snapshot := LoadSubscriptionMetricsSnapshot(ctx, now)
		connect.AssertEqual(t, snapshot.UncreditedPayments, map[string]int64{
			CryptoAssetUsdcSolana: 1,
			CryptoAssetXmr:        1,
			CryptoAssetZec:        1,
		})
		ageNear := func(rail string, want time.Duration) {
			age := time.Duration(snapshot.UncreditedOldestAgeSeconds[rail]) * time.Second
			if age < want-time.Minute || want+time.Minute < age {
				t.Fatalf("%s oldest age = %s, want %s", rail, age, want)
			}
		}
		ageNear(CryptoAssetUsdcSolana, 2*time.Hour)
		ageNear(CryptoAssetXmr, time.Hour)
		ageNear(CryptoAssetZec, 3*time.Hour)

		// the payments are rows of their own ledgers, not of the snapshot:
		// nothing was credited, removed or changed by counting them
		_, err := GetUnfulfilledSolanaPayment(ctx, "uncredited-old")
		connect.AssertEqual(t, err, nil)
	})
}

// The uncredited query reads the three ledgers, names a closed rail set, and
// exports only rails, counts and ages.
func TestSubscriptionMetricsUncreditedQueryContract(t *testing.T) {
	for _, required := range []string{
		"FROM solana_unfulfilled_payment unfulfilled",
		"COALESCE(unfulfilled.transaction_time, unfulfilled.record_time)",
		"FROM crypto_payment_receipt receipt",
		"receipt.state IN ('seen', 'confirmed', 'review')",
		"FROM gamma_charge charge",
		"charge.state = 'spent'",
		"intent.credited_at IS NULL",
		"intent.voided_at IS NULL",
		"WHERE received_at <= $2",
		"rail IN (" + subscriptionMetricsSqlList(SubscriptionMetricsUncreditedRails()) + ")",
	} {
		if !strings.Contains(subscriptionMetricsUncreditedQuery, required) {
			t.Errorf("uncredited query is missing %q", required)
		}
	}
	rails := SubscriptionMetricsUncreditedRails()
	for _, asset := range CryptoAssets {
		if !slices.Contains(rails, asset) {
			t.Errorf("uncredited rails %v omit the crypto asset %s", rails, asset)
		}
	}
	if !slices.Contains(rails, SubscriptionMetricsRailGamma) {
		t.Errorf("uncredited rails %v omit gamma", rails)
	}
	selectTail := subscriptionMetricsUncreditedQuery[strings.LastIndex(subscriptionMetricsUncreditedQuery, "SELECT "):]
	for _, identifier := range []string{
		"network_id", "tx_signature", "tx_id", "intent_id", "sender_account", "address", "reference",
	} {
		if strings.Contains(selectTail, identifier) {
			t.Errorf("uncredited result tail exports identifier %q: %s", identifier, selectTail)
		}
	}
}
