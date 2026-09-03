package controller

import (
	"context"
	"testing"
	"time"

	"github.com/mr-tron/base58"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Database-backed test for the USDC on Solana data pack path
// (pay_data_solana_controller.go). Needs the test database
// (server.DefaultTestEnv) and pro.yml. The hermetic pieces are in
// pay_data_solana_test.go.

// solanaTestMemoPayment is a USDC transfer to our receiving address that does
// NOT carry the reference as an account key: it was sent by hand from a wallet
// or an exchange, with the reference as the transfer memo (a memo program
// instruction whose data Helius carries base58 encoded).
func solanaTestMemoPayment(reference string, signature string, amountUsd float64) *SolanaTransaction {
	return &SolanaTransaction{
		Type:      "TRANSFER",
		Signature: signature,
		TokenTransfers: []TokenTransfer{
			{
				Mint:            solanaUsdcMint,
				FromUserAccount: "ExchangeHotWa11etBBBBBBBBBBBBBBBBBBBBBBBBBB",
				ToUserAccount:   solanaReceiverAddresses[0],
				TokenAmount:     amountUsd,
			},
		},
		AccountData: []AccountData{
			{Account: "ExchangeHotWa11etBBBBBBBBBBBBBBBBBBBBBBBBBB"},
			{Account: solanaReceiverAddresses[0]},
			{Account: "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
			{Account: solanaMemoProgramIds[1]},
		},
		Instructions: []Instruction{
			{
				ProgramId: "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
				Data:      "3DZDBbA1qEaC",
			},
			{
				ProgramId: solanaMemoProgramIds[1],
				Data:      base58.Encode([]byte(reference)),
			},
		},
	}
}

// TestSolanaWebhookAppliesDataPackToNamedNetwork walks a USDC data pack purchase
// through the real path: the buy-data page registers an intent for a NAMED
// network with no sign-in and no email, the buyer sends the quoted amount by
// hand with the reference as the memo, and the webhook lands the data on that
// network: data only (no Pro), valid for the data code duration, the intent
// consumed so a redelivery cannot land it twice. The admin's login is an email,
// so the applied note is sent there.
func TestSolanaWebhookAppliesDataPackToNamedNetwork(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		adminEmail := model.Testing_CreateNetwork(ctx, networkId, "buydatasolana", userId)

		// the buy-data page is not signed in: a session with no jwt
		pageSession := session.Testing_CreateClientSession(ctx, nil)
		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		sentTo := []string{}
		var sentTemplate Template
		prevSender := GetAWSMessageSender()
		SetMessageSender(&mockAWSMessageSender{
			SendMessageFunc: func(userAuth string, template Template, sendOpts ...any) error {
				sentTo = append(sentTo, userAuth)
				sentTemplate = template
				return nil
			},
		})
		defer SetMessageSender(prevSender)

		reference := "7Gk3sQx9pLmN2vB4cD6eF8hJ1kM5nP7rS9tU2wY4zA6B"
		intentResult, err := PayDataSolanaIntent(&PayDataSolanaIntentArgs{
			ItemId:      StripeItemData1Tib,
			NetworkName: "BuyDataSolana",
			Reference:   reference,
		}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, intentResult.Error, nil)
		priceUsd, ok := dataPackPriceUsd(StripeItemData1Tib)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, intentResult.AmountUsd, priceUsd)
		connect.AssertEqual(t, intentResult.Memo, reference)
		connect.AssertEqual(t, intentResult.NetworkName, "buydatasolana")
		connect.AssertEqual(t, *intentResult.NetworkId, networkId)
		connect.AssertEqual(t, intentResult.ExpiresAt.Sub(server.NowUtc()) > 23*time.Hour, true)

		// the same reference cannot be quoted twice
		dup, err := PayDataSolanaIntent(&PayDataSolanaIntentArgs{
			ItemId:      StripeItemData10Tib,
			NetworkName: "buydatasolana",
			Reference:   reference,
		}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, dup.Error != nil, true)

		status, err := PayDataSolanaStatus(&PayDataSolanaStatusArgs{Reference: reference}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, PayDataSolanaStatusPending)
		connect.AssertEqual(t, status.ItemId, StripeItemData1Tib)
		connect.AssertEqual(t, status.NetworkName, "buydatasolana")
		connect.AssertEqual(t, status.AmountUsd, priceUsd)

		// underpaying buys nothing and keeps the intent open
		result, err := HeliusWebhook(
			[]*SolanaTransaction{solanaTestMemoPayment(reference, "sig-datapack-under", priceUsd-1)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Message, "Payment is less than the quoted price")
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)

		// paid by hand: the reference is only in the memo, not among the accounts
		result, err = HeliusWebhook(
			[]*SolanaTransaction{solanaTestMemoPayment(reference, "sig-datapack-1", priceUsd)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Message, "Processed 1 matching payments")

		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].BalanceByteCount, 1*model.Tib)
		connect.AssertEqual(t, balances[0].Pro, false)
		connect.AssertEqual(t, balances[0].EndTime.Sub(balances[0].StartTime), model.Pro().DataCodeDuration)
		connect.AssertEqual(t, balances[0].NetRevenue, model.UsdToNanoCents(priceUsd))
		// data only: buying data never confers Pro
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)

		// the applied note went to the network's admin email, with no code to show
		connect.AssertEqual(t, sentTo, []string{adminEmail})
		applied, ok := sentTemplate.(*SubscriptionDataAppliedTemplate)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, applied.NetworkName, "buydatasolana")
		connect.AssertEqual(t, applied.BalanceByteCount, 1*model.Tib)
		connect.AssertEqual(t, applied.Secret, "")

		status, err = PayDataSolanaStatus(&PayDataSolanaStatusArgs{Reference: reference}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, PayDataSolanaStatusPaid)

		// Helius redelivers the same transaction: the intent is consumed, nothing
		// lands twice and no second note is sent
		result, err = HeliusWebhook(
			[]*SolanaTransaction{solanaTestMemoPayment(reference, "sig-datapack-1", priceUsd)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Message, "No payment intent found for this network ID")
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		connect.AssertEqual(t, len(sentTo), 1)

		// a plan intent is not reported by the public status endpoint
		unknown, err := PayDataSolanaStatus(&PayDataSolanaStatusArgs{Reference: "never-quoted-reference"}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, unknown.Status, PayDataSolanaStatusUnknown)
	})
}
