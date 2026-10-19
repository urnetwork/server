package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// TestSolanaUniqueAmountMicro pins the quote arithmetic: the price plus a
// suffix of 1 to 9999 micro-USDC, exact in USDC's 6 decimals and always under
// a cent above the price.
func TestSolanaUniqueAmountMicro(t *testing.T) {
	amountMicro, ok := model.SolanaUniqueAmountMicro(40, 4317)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, amountMicro, int64(40_004_317))
	connect.AssertEqual(t, model.SolanaMicroToUsd(amountMicro), 40.004317)
	connect.AssertEqual(t, solanaFormatMicro(amountMicro), "40.004317")

	// a price with cents
	amountMicro, ok = model.SolanaUniqueAmountMicro(4.99, 1)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, amountMicro, int64(4_990_001))

	// the largest suffix stays under a cent
	amountMicro, ok = model.SolanaUniqueAmountMicro(5, model.SolanaUniqueAmountMaxSuffixMicro)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, amountMicro, int64(5_009_999))

	// no zero suffix: the plain price never looks like a unique quote
	_, ok = model.SolanaUniqueAmountMicro(5, 0)
	connect.AssertEqual(t, ok, false)
	_, ok = model.SolanaUniqueAmountMicro(5, model.SolanaUniqueAmountMaxSuffixMicro+1)
	connect.AssertEqual(t, ok, false)
	_, ok = model.SolanaUniqueAmountMicro(0, 1)
	connect.AssertEqual(t, ok, false)

	// the chain reports the amount as a float; the round trip is exact
	connect.AssertEqual(t, model.SolanaUsdToMicro(40.004317), int64(40_004_317))
	connect.AssertEqual(t, model.SolanaUsdToMicro(0.1+0.2), int64(300_000))
	connect.AssertEqual(t, model.SolanaUsdToMicro(9_999.999999), int64(9_999_999_999))
}

// TestSolanaSuffixPickerStaysInRange: the production picker only proposes
// suffixes the quote accepts.
func TestSolanaSuffixPickerStaysInRange(t *testing.T) {
	for range 10_000 {
		suffixMicro := solanaPickAmountSuffixMicro()
		if suffixMicro < 1 || model.SolanaUniqueAmountMaxSuffixMicro < suffixMicro {
			t.Fatalf("suffix %d out of range", suffixMicro)
		}
	}
}

// TestSolanaMemolessRecordCarriesWhatSupportNeeds: a memo-less payment that is
// not credited is recorded with the sender and the reason, next to the
// signature, amount and time.
func TestSolanaMemolessRecordCarriesWhatSupportNeeds(t *testing.T) {
	sentAt := solanaMemolessQuoteTime.Add(10 * time.Minute)
	lookups := &solanaFakePaymentLookups{
		intents: []*model.SolanaPaymentIntent{
			solanaMemolessIntent("support-a", server.NewId(), 40.004317),
			solanaMemolessIntent("support-b", server.NewId(), 40.004317),
		},
	}
	decision := solanaDecidePayment(
		solanaMemolessPayment("sig-support", solanaReceiverAddresses[0], 40.004317, sentAt),
		lookups,
	)
	connect.AssertEqual(t, decision.action, solanaPaymentActionRecord)
	connect.AssertEqual(t, decision.unfulfilled.TxSignature, "sig-support")
	connect.AssertEqual(t, decision.unfulfilled.TokenAmountUsd, 40.004317)
	connect.AssertEqual(t, *decision.unfulfilled.TransactionTime, sentAt)
	connect.AssertEqual(t, *decision.unfulfilled.SenderAccount, solanaMemolessSender)
	connect.AssertEqual(t, *decision.unfulfilled.MatchNote, "memo-less: ambiguous: 2 intents were quoted 40.004317 USDC: support-a, support-b")
	// nothing points the record at one network: support decides
	connect.AssertEqual(t, decision.unfulfilled.NetworkId, (*server.Id)(nil))
	connect.AssertEqual(t, decision.unfulfilled.PaymentReference, (*string)(nil))
}

// TestSolanaMemolessWindowEdges: the window is [created_at - skew, expires_at],
// both ends inclusive.
func TestSolanaMemolessWindowEdges(t *testing.T) {
	intent := solanaMemolessIntent("edge", server.NewId(), 5.000001)
	transferAt := func(sentAt time.Time) *solanaMemolessTransfer {
		return &solanaMemolessTransfer{
			amountMicro:     5_000_001,
			tokenAmountUsd:  5.000001,
			senderAccount:   solanaMemolessSender,
			transactionTime: sentAt,
		}
	}
	candidates := []*model.SolanaPaymentIntent{intent}

	matched, _ := solanaMatchMemolessIntent(transferAt(intent.CreatedAt.Add(-solanaMemolessClockSkew)), candidates)
	connect.AssertEqual(t, matched, intent)
	matched, _ = solanaMatchMemolessIntent(transferAt(*intent.ExpiresAt), candidates)
	connect.AssertEqual(t, matched, intent)

	matched, _ = solanaMatchMemolessIntent(transferAt(intent.CreatedAt.Add(-solanaMemolessClockSkew-time.Second)), candidates)
	connect.AssertEqual(t, matched, (*model.SolanaPaymentIntent)(nil))
	matched, _ = solanaMatchMemolessIntent(transferAt(intent.ExpiresAt.Add(time.Second)), candidates)
	connect.AssertEqual(t, matched, (*model.SolanaPaymentIntent)(nil))

	// a stored quote that is not the amount (a legacy row) never matches
	legacy := solanaMemolessIntent("legacy", server.NewId(), 5)
	matched, note := solanaMatchMemolessIntent(transferAt(intent.CreatedAt), []*model.SolanaPaymentIntent{legacy})
	connect.AssertEqual(t, matched, (*model.SolanaPaymentIntent)(nil))
	connect.AssertNotEqual(t, note, "")
}
