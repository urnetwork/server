package controller

import (
	"context"
	"testing"
	"time"

	"github.com/mr-tron/base58"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Hermetic tests for the USDC on Solana data pack path: the quote, the
// reference shape, the status mapping and the webhook's reference candidates.
// The end-to-end credit is TestSolanaWebhookAppliesDataPackToNamedNetwork in
// pay_data_checkout_db_test.go.

// TestDataPackPriceUsd pins that the USDC quote is the pro.yml price -- the same
// number the site shows and Stripe charges -- never a client-supplied amount.
func TestDataPackPriceUsd(t *testing.T) {
	skipWithoutProYml(t)

	price, ok := dataPackPriceUsd(StripeItemData1Tib)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, price, float64(3))

	price, ok = dataPackPriceUsd(StripeItemData10Tib)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, price, float64(20))

	// plans and unknown items have no data pack price
	_, ok = dataPackPriceUsd(StripeItemProMonthly)
	connect.AssertEqual(t, ok, false)
	_, ok = dataPackPriceUsd("data_2tib")
	connect.AssertEqual(t, ok, false)
	_, ok = dataPackPriceUsd("")
	connect.AssertEqual(t, ok, false)

	// the webhook tells a data pack intent from a plan by the same table
	connect.AssertEqual(t, solanaIsDataPackPlan(StripeItemData1Tib), true)
	connect.AssertEqual(t, solanaIsDataPackPlan(StripeItemData10Tib), true)
	connect.AssertEqual(t, solanaIsDataPackPlan(model.SolanaPlanMonthly), false)
	connect.AssertEqual(t, solanaIsDataPackPlan(model.SolanaPlanYearly), false)
	connect.AssertEqual(t, solanaIsDataPackPlan(""), false)
}

func TestPayDataSolanaValidReference(t *testing.T) {
	// a Solana Pay reference: a base58 public key
	connect.AssertEqual(t, payDataSolanaValidReference("7Gk3sQx9pLmN2vB4cD6eF8hJ1kM5nP7rS9tU2wY4zA6B"), true)
	connect.AssertEqual(t, payDataSolanaValidReference("abcdefgh"), true)
	connect.AssertEqual(t, payDataSolanaValidReference("abcdefg"), false)
	connect.AssertEqual(t, payDataSolanaValidReference(""), false)
	connect.AssertEqual(t, payDataSolanaValidReference("has a space in it"), false)
	connect.AssertEqual(t, payDataSolanaValidReference("has\ttab-in-it"), false)
	connect.AssertEqual(t, payDataSolanaValidReference("non-ascii-référence"), false)
	long := ""
	for len(long) <= payDataSolanaReferenceMaxLength {
		long += "x"
	}
	connect.AssertEqual(t, payDataSolanaValidReference(long), false)
}

// TestPayDataSolanaStatusOf pins the status the page shows for each intent state.
func TestPayDataSolanaStatusOf(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	signature := "sig-1"

	connect.AssertEqual(t, payDataSolanaStatusOf(&model.SolanaPaymentIntent{ExpiresAt: &future}, now), PayDataSolanaStatusPending)
	connect.AssertEqual(t, payDataSolanaStatusOf(&model.SolanaPaymentIntent{}, now), PayDataSolanaStatusPending)
	connect.AssertEqual(t, payDataSolanaStatusOf(&model.SolanaPaymentIntent{ExpiresAt: &past}, now), PayDataSolanaStatusExpired)
	// a late payment still credits, so a consumed intent is paid whatever its expiry
	connect.AssertEqual(t, payDataSolanaStatusOf(&model.SolanaPaymentIntent{ExpiresAt: &past, TxSignature: &signature}, now), PayDataSolanaStatusPaid)
	connect.AssertEqual(t, payDataSolanaStatusOf(&model.SolanaPaymentIntent{ExpiresAt: &future, TxSignature: &signature}, now), PayDataSolanaStatusPaid)
}

// TestPayDataSolanaIntentEarlyErrors pins the refusals that happen before any
// lookup: nothing here touches the database.
func TestPayDataSolanaIntentEarlyErrors(t *testing.T) {
	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	reference := "7Gk3sQx9pLmN2vB4cD6eF8hJ1kM5nP7rS9tU2wY4zA6B"

	result, err := PayDataSolanaIntent(&PayDataSolanaIntentArgs{ItemId: "data_2tib", NetworkName: "net", Reference: reference}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error.Message, "Unknown item.")

	result, err = PayDataSolanaIntent(&PayDataSolanaIntentArgs{ItemId: StripeItemProMonthly, NetworkName: "net", Reference: reference}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error.Message, "Unknown item.")

	result, err = PayDataSolanaIntent(&PayDataSolanaIntentArgs{ItemId: StripeItemData1Tib, NetworkName: "net", Reference: "short"}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error.Message, "Invalid payment reference.")

	// no network means nowhere for the data to go: crypto never asks for an email
	result, err = PayDataSolanaIntent(&PayDataSolanaIntentArgs{ItemId: StripeItemData1Tib, NetworkName: "  ", Reference: reference}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error.Message, "Enter the network that should receive the data.")

	// a malformed reference is simply unknown to the status endpoint
	status, err := PayDataSolanaStatus(&PayDataSolanaStatusArgs{Reference: " "}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, status.Status, PayDataSolanaStatusUnknown)
}

// TestSolanaReferenceCandidates pins what the webhook searches an intent under:
// every account key (a Solana Pay wallet attaches the reference as one) and
// every memo text (a payment sent by hand carries it as the transfer memo),
// from either memo program, top-level or inner, decoded from base58 and kept
// verbatim, without duplicates and in order.
func TestSolanaReferenceCandidates(t *testing.T) {
	reference := "7Gk3sQx9pLmN2vB4cD6eF8hJ1kM5nP7rS9tU2wY4zA6B"
	other := "AnotherRef9pLmN2vB4cD6eF8hJ1kM5nP7rS9tU2wY4z"

	transaction := &SolanaTransaction{
		AccountData: []AccountData{
			{Account: "CustomerWa11etAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
			{Account: solanaReceiverAddresses[0]},
			{Account: " "},
			{Account: solanaReceiverAddresses[0]},
		},
		Instructions: []Instruction{
			// the token transfer itself: not a memo, contributes nothing
			{ProgramId: "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA", Data: base58.Encode([]byte("ignored"))},
			// memo v2, base58 encoded as Helius carries it, with surrounding whitespace
			{ProgramId: solanaMemoProgramIds[1], Data: base58.Encode([]byte("  " + reference + "\n"))},
			// memo v1 inside a wrapped instruction
			{
				ProgramId: "ComputeBudget111111111111111111111111111111",
				InnerInstructions: []InnerInstruction{
					{ProgramId: solanaMemoProgramIds[0], Data: base58.Encode([]byte(other))},
				},
			},
			// the same memo again: no duplicate
			{ProgramId: solanaMemoProgramIds[1], Data: base58.Encode([]byte(reference))},
		},
	}

	candidates := solanaReferenceCandidates(transaction)
	connect.AssertEqual(t, candidates, []string{
		"CustomerWa11etAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		solanaReceiverAddresses[0],
		reference,
		base58.Encode([]byte("  " + reference + "\n")),
		other,
		base58.Encode([]byte(other)),
		base58.Encode([]byte(reference)),
	})

	// a memo that is not base58 is kept verbatim, so a payload carrying the
	// memo text as-is still matches
	verbatim := &SolanaTransaction{
		Instructions: []Instruction{
			{ProgramId: solanaMemoProgramIds[0], Data: reference + "!"},
		},
	}
	connect.AssertEqual(t, solanaReferenceCandidates(verbatim), []string{reference + "!"})

	// no memo, no accounts: nothing to search
	connect.AssertEqual(t, solanaReferenceCandidates(&SolanaTransaction{}), []string{})

	// memo data that decodes to bytes that are not text is not offered decoded
	binary := &SolanaTransaction{
		Instructions: []Instruction{
			{ProgramId: solanaMemoProgramIds[1], Data: base58.Encode([]byte{0xff, 0xfe, 0x00, 0x01})},
		},
	}
	connect.AssertEqual(t, solanaReferenceCandidates(binary), []string{base58.Encode([]byte{0xff, 0xfe, 0x00, 0x01})})

	// the existing plan flow is unchanged: the reference among the accounts
	planPayment := solanaTestPayment(reference, "sig", 5)
	found := false
	for _, candidate := range solanaReferenceCandidates(planPayment) {
		if candidate == reference {
			found = true
		}
	}
	connect.AssertEqual(t, found, true)
	_ = server.NowUtc()
}
