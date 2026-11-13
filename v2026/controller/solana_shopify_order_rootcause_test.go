package controller

// The root cause (owner, 2026-11-13): Shopify's Solana Pay app pays our
// store's URnode orders into the same USDC receiver the apps' purchases pay,
// and the Helius webhook and the reconcile's receiver leg recorded each such
// payment as no_intent, which pages payment-solana-unfulfilled. A payment
// whose Shopify order record verifies it is recorded as the order's payment
// instead: never unfulfilled, never credited. Hermetic: the webhook's and the
// leg's seams, the fake RPC of solana_receiver_fake_test.go, and the store's
// order record of solanaShopifyTestShop (solana_shopify_order_test.go). These
// tests name only what existed before the fix but that one helper, so they
// also run, and fail, against the code before it.

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The accounts of a synthetic payment as Shopify's Solana Pay app builds it:
// every one synthetic but the app's payment program (Helio's), the token
// program and the USDC mint, which are the chain's.
const (
	solanaShopifyTestPayProgram  = "ENicYBBNZQ91toN7ggmTxnDGZW14uv9UkumN7XBGeYJ4"
	solanaShopifyTestMemoProgram = "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr"
	solanaShopifyTestAppPayer    = "SynthAppFeePayer222222222222222222222222222"
	solanaShopifyTestBuyer       = "SynthBuyerWa11et33333333333333333333333333"
	solanaShopifyTestBuyerToken  = "SynthBuyerToken444444444444444444444444444"
	solanaShopifyTestFeeAToken   = "SynthAppFeeAToken5555555555555555555555555"
	solanaShopifyTestFeeAOwner   = "SynthAppFeeAOwner6666666666666666666666666"
	solanaShopifyTestFeeBToken   = "SynthAppFeeBToken7777777777777777777777777"
	solanaShopifyTestFeeBOwner   = "SynthAppFeeBOwner8888888888888888888888888"
	// a payment session id and a hash, the shape of the app's memo
	solanaShopifyTestMemo = "6a0b1c2d3e4f5a6b7c8d9e0f-c3ludGhldGljLXNob3BpZnktbWVtby1oYXNoLTE="
)

// A finalized payment as the app builds it, in the jsonParsed shape the
// receiver leg reads: the app's program splits the buyer's USDC between our
// receiver (share) and the app's two fee accounts (feeA, feeB), the app pays
// the fee, and a memo names the app's session.
func solanaShopifyTestParsedPayment(signature string, slot uint64, blockTime time.Time, receiver string, receiverToken string, share int64, feeA int64, feeB int64) *solanaReceiverFakeTransaction {
	const buyerBefore = 1_000_000_000
	total := share + feeA + feeB
	transferChecked := func(destination string, amount int64) map[string]any {
		return map[string]any{
			"program":   "spl-token",
			"programId": solanaReceiverFakeTokenProgram,
			"parsed": map[string]any{
				"type": "transferChecked",
				"info": map[string]any{
					"authority":   solanaShopifyTestBuyer,
					"source":      solanaShopifyTestBuyerToken,
					"destination": destination,
					"mint":        solanaUsdcMint,
					"tokenAmount": map[string]any{"amount": strconv.FormatInt(amount, 10), "decimals": 6},
				},
			},
		}
	}
	key := func(pubkey string, signer bool) map[string]any {
		return map[string]any{"pubkey": pubkey, "signer": signer}
	}
	return &solanaReceiverFakeTransaction{
		signature: signature,
		slot:      slot,
		blockTime: blockTime,
		parsed: map[string]any{
			"slot":      slot,
			"blockTime": blockTime.Unix(),
			"meta": map[string]any{
				"err": nil,
				"innerInstructions": []any{
					map[string]any{
						"index": 0,
						"instructions": []any{
							transferChecked(receiverToken, share),
							transferChecked(solanaShopifyTestFeeAToken, feeA),
							transferChecked(solanaShopifyTestFeeBToken, feeB),
						},
					},
				},
				"preTokenBalances": []any{
					solanaReceiverFakeBalance(2, solanaShopifyTestBuyer, buyerBefore),
					solanaReceiverFakeBalance(3, receiver, 0),
					solanaReceiverFakeBalance(8, solanaShopifyTestFeeAOwner, 5_000_000_000),
					solanaReceiverFakeBalance(9, solanaShopifyTestFeeBOwner, 7_000_000_000),
				},
				"postTokenBalances": []any{
					solanaReceiverFakeBalance(2, solanaShopifyTestBuyer, buyerBefore-total),
					solanaReceiverFakeBalance(3, receiver, share),
					solanaReceiverFakeBalance(8, solanaShopifyTestFeeAOwner, 5_000_000_000+feeA),
					solanaReceiverFakeBalance(9, solanaShopifyTestFeeBOwner, 7_000_000_000+feeB),
				},
			},
			"transaction": map[string]any{
				"signatures": []any{signature},
				"message": map[string]any{
					"accountKeys": []any{
						key(solanaShopifyTestAppPayer, true),
						key(solanaShopifyTestBuyer, true),
						key(solanaShopifyTestBuyerToken, false),
						key(receiverToken, false),
						key(solanaShopifyTestPayProgram, false),
						key(receiver, false),
						key("ComputeBudget111111111111111111111111111111", false),
						key(solanaShopifyTestMemoProgram, false),
						key(solanaShopifyTestFeeAToken, false),
						key(solanaShopifyTestFeeBToken, false),
						key(solanaShopifyTestFeeAOwner, false),
						key(solanaShopifyTestFeeBOwner, false),
						key(solanaUsdcMint, false),
						key(solanaReceiverFakeTokenProgram, false),
					},
					"instructions": []any{
						map[string]any{"programId": solanaShopifyTestPayProgram, "accounts": []any{solanaShopifyTestBuyer, solanaShopifyTestBuyerToken, receiverToken}, "data": "2PvhrZru6GfAp9Rh"},
						map[string]any{"programId": "ComputeBudget111111111111111111111111111111", "data": "3QAwFKa3MJAs"},
						map[string]any{"program": "spl-memo", "programId": solanaShopifyTestMemoProgram, "parsed": solanaShopifyTestMemo},
					},
				},
			},
		},
	}
}

// The same payment as Helius's enhanced transaction delivers it to the
// webhook: its token transfers, each with its source and destination token
// accounts, its instructions and its memo.
func solanaShopifyTestHeliusPayment(signature string, blockTime time.Time, receiver string, receiverToken string, share int64, feeA int64, feeB int64) *SolanaTransaction {
	transfer := func(destinationToken string, destinationOwner string, amount int64) TokenTransfer {
		uiAmount, _ := strconv.ParseFloat(cryptoFormatAtomic(amount, 6), 64)
		return TokenTransfer{
			FromTokenAccount: solanaShopifyTestBuyerToken,
			FromUserAccount:  solanaShopifyTestBuyer,
			Mint:             solanaUsdcMint,
			ToTokenAccount:   destinationToken,
			ToUserAccount:    destinationOwner,
			TokenAmount:      uiAmount,
			TokenStandard:    "Fungible",
		}
	}
	accounts := []AccountData{}
	for _, account := range []string{solanaShopifyTestAppPayer, solanaShopifyTestBuyer, solanaShopifyTestBuyerToken, receiverToken, solanaShopifyTestPayProgram, receiver, solanaShopifyTestMemoProgram} {
		accounts = append(accounts, AccountData{Account: account})
	}
	return &SolanaTransaction{
		Signature:   signature,
		Slot:        1,
		Timestamp:   blockTime.Unix(),
		Type:        "TRANSFER",
		FeePayer:    solanaShopifyTestAppPayer,
		AccountData: accounts,
		TokenTransfers: []TokenTransfer{
			transfer(receiverToken, receiver, share),
			transfer(solanaShopifyTestFeeAToken, solanaShopifyTestFeeAOwner, feeA),
			transfer(solanaShopifyTestFeeBToken, solanaShopifyTestFeeBOwner, feeB),
		},
		Instructions: []Instruction{
			{ProgramId: solanaShopifyTestPayProgram, Accounts: []string{solanaShopifyTestBuyer, solanaShopifyTestBuyerToken, receiverToken}, Data: "2PvhrZru6GfAp9Rh"},
			{ProgramId: "ComputeBudget111111111111111111111111111111", Data: "3QAwFKa3MJAs"},
			{ProgramId: solanaShopifyTestMemoProgram, Data: solanaShopifyTestMemo},
		},
	}
}

// The webhook records a Shopify order's Solana Pay payment as the order's:
// not unfulfilled (no page), never credited.
func TestHeliusWebhookRecordsAShopifyOrdersPaymentAsTheOrders(t *testing.T) {
	receiver := "SynthReceiverWa11et99999999999999999999999"
	receiverToken := "SynthReceiverToken9999999999999999999999999"
	previousReceivers := solanaReceiverAddresses
	solanaReceiverAddresses = []string{receiver}
	t.Cleanup(func() { solanaReceiverAddresses = previousReceivers })
	store := &solanaFakeUnfulfilledStore{searchResults: map[string]*model.PaymentIntentSearchResult{}}
	store.install(t)
	blockTime := time.Date(2026, 11, 13, 10, 15, 2, 0, time.UTC)
	// an order of 143.97 USD, its sale processed 50 s before the block
	recorded := solanaShopifyTestShop(t, "#1001", 14397, blockTime.Add(-50*time.Second))

	// 142.5303 USDC to us, 1.29573 and 0.14397 to the app's fee accounts
	transaction := solanaShopifyTestHeliusPayment("shopify-order-paid", blockTime, receiver, receiverToken, 142_530_300, 1_295_730, 143_970)
	_, err := HeliusWebhook([]*SolanaTransaction{transaction}, &session.ClientSession{Ctx: context.Background()})
	connect.AssertEqual(t, err, nil)
	if len(store.recorded) != 0 {
		t.Fatalf("a Shopify order's Solana Pay payment was recorded unfulfilled (%s), which pages payment-solana-unfulfilled", store.recorded[0].Reason)
	}
	connect.AssertEqual(t, recorded(), map[string]string{"shopify-order-paid": "#1001"})
}

// The receiver leg records a Shopify order's Solana Pay payment as the
// order's and moves its cursor past it: not unfulfilled, never credited.
func TestSolanaReceiverLegRecordsAShopifyOrdersPaymentAsTheOrders(t *testing.T) {
	test := newSolanaReceiverLegTest(t)
	blockTime := solanaReceiverTestNow.Add(-2 * time.Hour)
	recorded := solanaShopifyTestShop(t, "#1002", 14397, blockTime.Add(-90*time.Second))
	test.history(solanaShopifyTestParsedPayment("shopify-order-paid", 7, blockTime, solanaReceiverTestReceiver, solanaReceiverTestTokenAccount, 142_530_300, 1_295_730, 143_970))

	run := test.run(false)
	reconcileSolanaReceivers(run)
	if len(test.records) != 0 {
		t.Fatalf("a Shopify order's Solana Pay payment was recorded unfulfilled (%s), which pages payment-solana-unfulfilled", test.records[0].Reason)
	}
	connect.AssertEqual(t, len(test.credits), 0)
	connect.AssertEqual(t, recorded(), map[string]string{"shopify-order-paid": "#1002"})
	connect.AssertEqual(t, test.cursorSignature(), "shopify-order-paid")
	connect.AssertEqual(t, run.storeResult(model.SubscriptionMarketSolana).Errors, 0)
}
