package controller

// Root-cause tests, against the database, of support inbox 7247 for USDC on
// Solana: a USDC payment credited only when the Helius webhook delivered it,
// or within the hour when the reconcile's chain leg read it. The buyer's
// screen polls the status, and the status read only read the database, so a
// lost delivery kept a paid invoice "pending" for up to an hour. The status
// reads now read the chain for the open intent's reference and credit what
// they find through the webhook's own credit.
//
// The chain is a fake RPC answering the reference's signatures and a
// jsonParsed transfer to our receiver; the webhook is never called. Needs the
// test database and redis (server.DefaultTestEnv).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// solanaLostDeliveryRpc is the chain as Helius's RPC answers it: each
// address's finalized signatures, and each transaction, jsonParsed.
type solanaLostDeliveryRpc struct {
	signatures   map[string][]map[string]any
	transactions map[string]map[string]any
	calls        map[string]int
}

// Answers the reader's RPC calls from what the test put on the chain.
func (self *solanaLostDeliveryRpc) call(ctx context.Context, method string, params any, result any) error {
	self.calls[method] += 1
	args, _ := params.([]any)
	answer := func(value any) error {
		return (&fakeSolanaRpc{}).answer(value, result)
	}
	switch method {
	case "getSignaturesForAddress":
		address, _ := args[0].(string)
		if signatures, ok := self.signatures[address]; ok {
			return answer(signatures)
		}
		return answer([]any{})
	case "getTransaction":
		signature, _ := args[0].(string)
		if transaction, ok := self.transactions[signature]; ok {
			return answer(transaction)
		}
		return answer(nil)
	}
	return fmt.Errorf("unexpected method %s", method)
}

// Puts a finalized USDC transfer of amountMicro to our receiver on the chain,
// naming the reference as Solana Pay does (an account key of the transfer).
func (self *solanaLostDeliveryRpc) payUsdc(reference string, signature string, amountMicro int64) {
	payer := "CustomerWa11etAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	balance := func(index int, owner string, amount int64) map[string]any {
		return map[string]any{
			"accountIndex":  index,
			"mint":          solanaUsdcMint,
			"owner":         owner,
			"uiTokenAmount": map[string]any{"amount": fmt.Sprintf("%d", amount), "decimals": 6},
		}
	}
	self.signatures[reference] = []map[string]any{{
		"signature":          signature,
		"slot":               300000000,
		"err":                nil,
		"blockTime":          server.NowUtc().Unix(),
		"confirmationStatus": "finalized",
	}}
	self.transactions[signature] = map[string]any{
		"slot":      300000000,
		"blockTime": server.NowUtc().Unix(),
		"meta": map[string]any{
			"err":               nil,
			"innerInstructions": []any{},
			"preTokenBalances": []any{
				balance(1, payer, 100_000_000),
				balance(2, solanaReceiverAddresses[0], 0),
			},
			"postTokenBalances": []any{
				balance(1, payer, 100_000_000-amountMicro),
				balance(2, solanaReceiverAddresses[0], amountMicro),
			},
		},
		"transaction": map[string]any{
			"signatures": []string{signature},
			"message": map[string]any{
				"accountKeys": []any{
					map[string]any{"pubkey": payer, "signer": true},
					map[string]any{"pubkey": "PayerUsdcTokenAccountAAAAAAAAAAAAAAAAAAAAAA", "signer": false},
					map[string]any{"pubkey": "OurUsdcTokenAccountAAAAAAAAAAAAAAAAAAAAAAAA", "signer": false},
					map[string]any{"pubkey": reference, "signer": false},
				},
				"instructions": []any{},
			},
		},
	}
}

// The chain, installed as the reads' RPC until the test ends.
func newSolanaLostDeliveryRpc(t testing.TB) *solanaLostDeliveryRpc {
	rpc := &solanaLostDeliveryRpc{
		signatures:   map[string][]map[string]any{},
		transactions: map[string]map[string]any{},
		calls:        map[string]int{},
	}
	previousUrl := solanaChainRpcUrl
	previousRpcFor := solanaChainRpcFor
	solanaChainRpcUrl = func() (string, bool) { return "https://synthetic.invalid/?api-key=synthetic", true }
	solanaChainRpcFor = func(rpcUrl string) solanaChainRpc { return rpc }
	solanaParsedTransactions.reset()
	t.Cleanup(func() {
		solanaChainRpcUrl = previousUrl
		solanaChainRpcFor = previousRpcFor
		solanaParsedTransactions.reset()
	})
	return rpc
}

// A USDC invoice paid on the chain whose Helius delivery never came: the
// status read the buyer's screen polls finds the payment on the chain and
// credits the plan through the webhook's credit, once.
func TestCryptoUsdcStatusFindsAPaymentItsWebhookLost(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptousdclost")
		solanaTestAmountSuffixes(t, 311)
		rpc := newSolanaLostDeliveryRpc(t)

		result, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, Plan: model.SolanaPlanYearly}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*CryptoPaymentError)(nil))
		connect.AssertEqual(t, result.Amount, "40.000311")
		reference := result.Memo

		// the buyer paid a minute ago; Helius never delivered it
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET create_time = create_time - INTERVAL '2 minutes', quoted_at = quoted_at - INTERVAL '2 minutes' WHERE intent_id = $1`, server.RequireParseId(result.IntentId))
		cryptoDbExec(ctx, `UPDATE solana_payment_intent SET created_at = created_at - INTERVAL '2 minutes' WHERE payment_reference = $1`, reference)
		rpc.payUsdc(reference, "sig-usdc-lost-delivery", 40_000_311)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)

		status, err := CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: result.IntentId, StatusToken: result.StatusToken}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, CryptoPaymentStatusPaid)
		connect.AssertEqual(t, *model.GetSolanaPaymentIntent(ctx, reference).TxSignature, "sig-usdc-lost-delivery")
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, networkId), true)
		renewal := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketSolana)
		connect.AssertEqual(t, renewal.count, 1)
		connect.AssertEqual(t, renewal.transactionId, reference)

		// the screen keeps polling, and Helius's delivery arrives late: once
		status, err = CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: result.IntentId, StatusToken: result.StatusToken}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, CryptoPaymentStatusPaid)
		_, err = HeliusWebhook([]*SolanaTransaction{solanaTestPayment(reference, "sig-usdc-lost-delivery", 40.000311)}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketSolana).count, 1)
	})
}

// A USDC data purchase for a named network (ur.io's buy-data page): its
// status read finds the payment its webhook lost and applies the data.
func TestPayDataSolanaStatusFindsAPaymentItsWebhookLost(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		networkId, _, _ := cryptoDbTestNetwork(t, ctx, "paydatalost")
		solanaTestAmountSuffixes(t, 312)
		rpc := newSolanaLostDeliveryRpc(t)
		// the buy-data page's session: no sign-in
		anonymous := session.Testing_CreateClientSession(ctx, nil)

		reference := "Ref" + strings.ReplaceAll(server.NewId().String(), "-", "")
		intent, err := PayDataSolanaIntent(&PayDataSolanaIntentArgs{
			ItemId:      StripeItemData1Tib,
			Reference:   reference,
			NetworkName: "paydatalost",
		}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, intent.Error, (*PayDataCheckoutError)(nil))
		amountMicro := int64(intent.AmountUsd*1_000_000 + 0.5)

		cryptoDbExec(ctx, `UPDATE solana_payment_intent SET created_at = created_at - INTERVAL '2 minutes' WHERE payment_reference = $1`, reference)
		rpc.payUsdc(reference, "sig-paydata-lost-delivery", amountMicro)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)

		status, err := PayDataSolanaStatus(&PayDataSolanaStatusArgs{Reference: reference}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, PayDataSolanaStatusPaid)
		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].BalanceByteCount, model.Tib)
	})
}
