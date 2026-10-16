package controller

import (
	"math"
	"testing"
	"time"

	"github.com/mr-tron/base58"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The webhook's decision for a USDC payment sent WITHOUT the reference (a
// wallet that cannot add a memo), with the db replaced by an in-memory fake.
// Fixed clock, fixed amounts, no Postgres.

// solanaFakePaymentLookups is the intent table in memory, answering the same
// questions the db lookups answer.
type solanaFakePaymentLookups struct {
	intents []*model.SolanaPaymentIntent
}

func (self *solanaFakePaymentLookups) searchIntentsByReference(references []string) (*model.PaymentIntentSearchResult, error) {
	for _, intent := range self.intents {
		if intent.TxSignature != nil {
			continue
		}
		for _, reference := range references {
			if intent.PaymentReference == reference {
				networkId := intent.NetworkId
				return &model.PaymentIntentSearchResult{
					NetworkId:         &networkId,
					PaymentReference:  intent.PaymentReference,
					ExpectedAmountUsd: intent.ExpectedAmountUsd,
					SubscriptionPlan:  intent.SubscriptionPlan,
				}, nil
			}
		}
	}
	return nil, nil
}

func (self *solanaFakePaymentLookups) isPaymentCompleted(signature string) bool {
	for _, intent := range self.intents {
		if intent.TxSignature != nil && *intent.TxSignature == signature {
			return true
		}
	}
	return false
}

func (self *solanaFakePaymentLookups) intentsByAmountMicro(amountMicro int64, minExpiresAt time.Time) ([]*model.SolanaPaymentIntent, error) {
	intents := []*model.SolanaPaymentIntent{}
	for _, intent := range self.intents {
		if int64(math.Round(intent.ExpectedAmountUsd*1e6)) == amountMicro &&
			intent.ExpiresAt != nil && minExpiresAt.Before(*intent.ExpiresAt) {
			intents = append(intents, intent)
		}
	}
	return intents, nil
}

var solanaMemolessQuoteTime = time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)

const solanaMemolessSender = "Sender1111111111111111111111111111111111111"

func solanaMemolessIntent(reference string, networkId server.Id, amountUsd float64) *model.SolanaPaymentIntent {
	expiresAt := solanaMemolessQuoteTime.Add(time.Hour)
	return &model.SolanaPaymentIntent{
		PaymentReference:  reference,
		NetworkId:         networkId,
		ExpectedAmountUsd: amountUsd,
		SubscriptionPlan:  model.SolanaPlanYearly,
		CreatedAt:         solanaMemolessQuoteTime,
		ExpiresAt:         &expiresAt,
	}
}

// a hand-sent USDC transfer: sender, receiver and token program as account
// keys, no reference account and no memo
func solanaMemolessPayment(signature string, toAccount string, amountUsd float64, sentAt time.Time) *SolanaTransaction {
	return &SolanaTransaction{
		Type:      "TRANSFER",
		Signature: signature,
		Timestamp: sentAt.Unix(),
		TokenTransfers: []TokenTransfer{
			{
				Mint:            solanaUsdcMint,
				FromUserAccount: solanaMemolessSender,
				ToUserAccount:   toAccount,
				TokenAmount:     amountUsd,
			},
		},
		AccountData: []AccountData{
			{Account: solanaMemolessSender},
			{Account: toAccount},
			{Account: "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
		},
	}
}

// TestSolanaMemolessExactAmountIsCredited is the reported defect: a buyer whose
// wallet cannot add a memo sends exactly the quoted amount to our address
// inside the quote's window. The transfer carries no reference, so the webhook
// used to find no intent and only record the payment; the money bought nothing
// until support credited it by hand. The quote's unique amount identifies the
// one intent it pays.
func TestSolanaMemolessExactAmountIsCredited(t *testing.T) {
	networkId := server.NewId()
	lookups := &solanaFakePaymentLookups{
		intents: []*model.SolanaPaymentIntent{
			solanaMemolessIntent("memoless-ref-1", networkId, 40.004317),
			// another open quote of the same plan, a different unique amount
			solanaMemolessIntent("memoless-ref-2", server.NewId(), 40.000912),
		},
	}

	decision := solanaDecidePayment(
		solanaMemolessPayment("sig-memoless-1", solanaReceiverAddresses[0], 40.004317, solanaMemolessQuoteTime.Add(10*time.Minute)),
		lookups,
	)

	if decision.action != solanaPaymentActionCredit {
		t.Fatalf("memo-less exact-amount transfer inside the window was not credited: action=%d message=%q", decision.action, decision.skipMessage)
	}
	connect.AssertEqual(t, decision.intent.PaymentReference, "memoless-ref-1")
	connect.AssertEqual(t, *decision.intent.NetworkId, networkId)
	connect.AssertEqual(t, decision.intent.SubscriptionPlan, model.SolanaPlanYearly)
	connect.AssertEqual(t, decision.tokenAmountUsd, 40.004317)
}

// TestSolanaMemolessNotCreditedWhenAmbiguousOrOutsideTheQuote pins that only an
// unambiguous match is credited. Every other case is recorded for support with
// the signature, amount and time, and nothing is credited.
func TestSolanaMemolessNotCreditedWhenAmbiguousOrOutsideTheQuote(t *testing.T) {
	sentAt := solanaMemolessQuoteTime.Add(10 * time.Minute)
	consumedSignature := "sig-earlier"
	consumed := solanaMemolessIntent("memoless-consumed", server.NewId(), 40.004317)
	consumed.TxSignature = &consumedSignature

	memoPayment := solanaMemolessPayment("sig-memo", solanaReceiverAddresses[0], 40.004317, sentAt)
	memoPayment.Instructions = []Instruction{{
		ProgramId: "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr",
		Data:      base58.Encode([]byte("not-a-known-reference")),
	}}

	splitPayment := solanaMemolessPayment("sig-split", solanaReceiverAddresses[0], 20.002158, sentAt)
	splitPayment.TokenTransfers = append(splitPayment.TokenTransfers, TokenTransfer{
		Mint:            solanaUsdcMint,
		FromUserAccount: solanaMemolessSender,
		ToUserAccount:   solanaReceiverAddresses[1],
		TokenAmount:     20.002159,
	})

	noTimePayment := solanaMemolessPayment("sig-no-time", solanaReceiverAddresses[0], 40.004317, sentAt)
	noTimePayment.Timestamp = 0

	cases := []struct {
		name        string
		intents     []*model.SolanaPaymentIntent
		transaction *SolanaTransaction
	}{
		{
			name: "two open intents quoted the same amount",
			intents: []*model.SolanaPaymentIntent{
				solanaMemolessIntent("memoless-a", server.NewId(), 40.004317),
				solanaMemolessIntent("memoless-b", server.NewId(), 40.004317),
			},
			transaction: solanaMemolessPayment("sig-ambiguous", solanaReceiverAddresses[0], 40.004317, sentAt),
		},
		{
			name:        "one micro-USDC off the quote",
			intents:     []*model.SolanaPaymentIntent{solanaMemolessIntent("memoless-c", server.NewId(), 40.004317)},
			transaction: solanaMemolessPayment("sig-wrong-amount", solanaReceiverAddresses[0], 40.004318, sentAt),
		},
		{
			name:        "the plain price without the suffix",
			intents:     []*model.SolanaPaymentIntent{solanaMemolessIntent("memoless-d", server.NewId(), 40.004317)},
			transaction: solanaMemolessPayment("sig-plain-price", solanaReceiverAddresses[0], 40, sentAt),
		},
		{
			name:        "sent after the quote expired",
			intents:     []*model.SolanaPaymentIntent{solanaMemolessIntent("memoless-e", server.NewId(), 40.004317)},
			transaction: solanaMemolessPayment("sig-expired", solanaReceiverAddresses[0], 40.004317, solanaMemolessQuoteTime.Add(time.Hour+time.Second)),
		},
		{
			name:        "sent before the quote was made",
			intents:     []*model.SolanaPaymentIntent{solanaMemolessIntent("memoless-f", server.NewId(), 40.004317)},
			transaction: solanaMemolessPayment("sig-before", solanaReceiverAddresses[0], 40.004317, solanaMemolessQuoteTime.Add(-time.Hour)),
		},
		{
			name:        "the only intent quoted the amount was already paid",
			intents:     []*model.SolanaPaymentIntent{consumed},
			transaction: solanaMemolessPayment("sig-second-payment", solanaReceiverAddresses[0], 40.004317, sentAt),
		},
		{
			name: "an open intent and a paid intent quoted the same amount",
			intents: []*model.SolanaPaymentIntent{
				consumed,
				solanaMemolessIntent("memoless-g", server.NewId(), 40.004317),
			},
			transaction: solanaMemolessPayment("sig-open-and-paid", solanaReceiverAddresses[0], 40.004317, sentAt),
		},
		{
			name:        "a memo that matched no intent",
			intents:     []*model.SolanaPaymentIntent{solanaMemolessIntent("memoless-h", server.NewId(), 40.004317)},
			transaction: memoPayment,
		},
		{
			name: "two transfers to our addresses",
			intents: []*model.SolanaPaymentIntent{
				solanaMemolessIntent("memoless-i", server.NewId(), 20.002158),
			},
			transaction: splitPayment,
		},
		{
			name:        "no on-chain time",
			intents:     []*model.SolanaPaymentIntent{solanaMemolessIntent("memoless-j", server.NewId(), 40.004317)},
			transaction: noTimePayment,
		},
	}
	for _, c := range cases {
		decision := solanaDecidePayment(c.transaction, &solanaFakePaymentLookups{intents: c.intents})
		if decision.action != solanaPaymentActionRecord {
			t.Fatalf("%s: action=%d, want the payment recorded and not credited", c.name, decision.action)
		}
		connect.AssertEqual(t, decision.unfulfilled.Reason, model.SolanaUnfulfilledReasonNoIntent)
		connect.AssertEqual(t, decision.unfulfilled.TxSignature, c.transaction.Signature)
		connect.AssertNotEqual(t, decision.unfulfilled.TokenAmountUsd, float64(0))
		if c.transaction.Timestamp != 0 {
			connect.AssertEqual(t, decision.unfulfilled.TransactionTime.Unix(), c.transaction.Timestamp)
		}
	}
}

// TestSolanaMemolessUnknownReceiverIsIgnored: USDC of the quoted amount sent to
// an address that is not ours is not our payment.
func TestSolanaMemolessUnknownReceiverIsIgnored(t *testing.T) {
	lookups := &solanaFakePaymentLookups{
		intents: []*model.SolanaPaymentIntent{solanaMemolessIntent("memoless-k", server.NewId(), 40.004317)},
	}
	decision := solanaDecidePayment(
		solanaMemolessPayment("sig-elsewhere", "NotOurAddress111111111111111111111111111111", 40.004317, solanaMemolessQuoteTime.Add(10*time.Minute)),
		lookups,
	)
	connect.AssertEqual(t, decision.action, solanaPaymentActionSkip)
}

// TestSolanaMemolessReplayIsNotCreditedTwice: Helius redelivers a memo-less
// payment that already consumed its intent. The signature is credited, so the
// redelivery is skipped, not matched by amount again.
func TestSolanaMemolessReplayIsNotCreditedTwice(t *testing.T) {
	signature := "sig-memoless-replay"
	intent := solanaMemolessIntent("memoless-l", server.NewId(), 40.004317)
	intent.TxSignature = &signature
	lookups := &solanaFakePaymentLookups{
		intents: []*model.SolanaPaymentIntent{
			intent,
			// even with another open intent around, the replay credits nothing
			solanaMemolessIntent("memoless-m", server.NewId(), 40.000001),
		},
	}
	decision := solanaDecidePayment(
		solanaMemolessPayment(signature, solanaReceiverAddresses[0], 40.004317, solanaMemolessQuoteTime.Add(10*time.Minute)),
		lookups,
	)
	connect.AssertEqual(t, decision.action, solanaPaymentActionSkip)
}

// TestSolanaReferenceMatchStaysFirst: a payment that carries its reference is
// matched by the reference, as before, even when it is not the unique amount.
func TestSolanaReferenceMatchStaysFirst(t *testing.T) {
	networkId := server.NewId()
	lookups := &solanaFakePaymentLookups{
		intents: []*model.SolanaPaymentIntent{
			solanaMemolessIntent("reference-first", networkId, 40.004317),
		},
	}
	// the plain price from an older client, with the reference attached: the
	// tolerance still accepts it
	payment := solanaMemolessPayment("sig-reference", solanaReceiverAddresses[0], 40, solanaMemolessQuoteTime.Add(10*time.Minute))
	payment.AccountData = append(payment.AccountData, AccountData{Account: "reference-first"})
	decision := solanaDecidePayment(payment, lookups)
	connect.AssertEqual(t, decision.action, solanaPaymentActionCredit)
	connect.AssertEqual(t, decision.intent.PaymentReference, "reference-first")
	connect.AssertEqual(t, *decision.intent.NetworkId, networkId)
}
